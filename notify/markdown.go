package notify

import (
	"fmt"
	"strings"
)

// This file provides the message rendering shared by the "Markdown-family" channels
// (DingTalk, WeCom). Feishu uses card JSON, Telegram uses HTML, and email uses HTML,
// each rendered in its own adapter.

// maxAssetsShown is the maximum number of assets listed in a message. A single finding
// may be anchored to dozens of assets; listing them all would overwhelm the message with
// no informational value — nobody reads the domains past the fourth one in an IM.
const maxAssetsShown = 3

// maxSummaryRunes is the number of characters the summary is compressed to. An IM message
// is a "hint to go look at the details," not the report itself; the full content lives on
// the platform.
const maxSummaryRunes = 120

// markdownReservedBytes is reserved for the message header (digest line + severity
// distribution + a possible truncation hint) and footer (platform link). When packing
// whole items, this amount is deducted from the budget to guarantee the header and footer
// are never truncated — once they are, the reader can't even tell which batch this is or
// how many items weren't shown.
const markdownReservedBytes = 320

// markdownEscape escapes markdown metacharacters.
//
// Why this is essential: finding titles, summaries, types, and asset display names all
// come from **untrusted sources** — titles and summaries come from model output (the model
// reads responses from the target under test), and an asset's url is the full URL obtained
// by scanning (including a target-controlled query string). Without escaping, a finding
// whose title is
//
//	Login SQL injection\n[Urgent: click here to verify your account](http://attacker.tld)
//
// would render as a **clickable external link** in the security engineer's DingTalk/Feishu;
// and `![](http://attacker.tld/beacon)` would be fetched by the client at render time,
// effectively announcing "this finding has been viewed" and leaking the reader's IP. Even
// benign content — an injected bold span or blockquote — can push a serious finding below
// the fold.
//
// The escape set covers the title/link/emphasis/list/quote/strikethrough classes of
// characters that change structure or produce clickable elements. `\` must be handled
// first, or it would re-escape the backslashes added afterward.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText compresses untrusted text to a single line and escapes it, for use in the
// markdown body. Single-lining is the other half of escaping: a newline on its own can
// forge a new list item or blockquote, and escape characters don't stop it.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (the IM platform's title bar / card title), as
// the **unescaped original text**.
//
// Escaping is deliberately not done here: this title is shared by renderers across four
// contexts — the markdown body, Telegram's HTML, the Feishu card's plain_text, and the
// generic Webhook's JSON plus the email subject. Each context's escaping rules differ
// (markdown escaping placed into HTML leaves visible backslashes, and into JSON it
// corrupts the data), so escaping must be the responsibility of each output end; see
// writeItem / feishuItemLines / telegramEscape. Markdown escaping was once added in the
// shared function, and the result was visible backslashes like `\(1\)` in Telegram messages.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Vulnerability summary · %d total", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Vulnerability notification"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body, returning the body and the **number of items
// actually written**.
//
// The return value kept is the number of items genuinely delivered this time; the caller
// uses it to mark only the first kept items as delivered — items kept out by the channel's
// length limit must be left for the next batch, not marked successful along with the rest.
// This is exactly the source of "silent loss": the message gets truncated, but the
// delivery record shows the whole batch as delivered, and nowhere can anyone tell that the
// latter half was never sent.
//
// maxBytes<=0 means no limit.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// A single-item message is sent even if it's overlong (the final truncation
		// backstops it): partial information about one finding is better than sending none.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[View all on the platform](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro renders the opening of a digest message: time window, item count,
// and severity distribution. With these, whoever receives the digest can judge whether the
// batch needs immediate attention without clicking into the platform.
//
// items are the items **actually packed in**, and total is the number this batch should
// have. When the two differ, it must be stated explicitly "how many more are in the next
// message" — otherwise the reader assumes the number in the message header is the total,
// while the never-sent items that follow simply don't exist in the UI at all.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**In the last %d minutes, %d new findings**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**%d new findings**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, " (this message shows the first %d; the remaining %d continue in the next message)", len(items), extra)
	}
	// Give the distribution by severity so the reader sees at a glance whether there are
	// critical items. Only count the items **actually included in this message**, so that
	// "critical 3" matches the items that can be counted below.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem renders a single finding item.
//
// prefix is the sequence number for the digest list; when single=true it renders the full
// version (including summary and back-link), while in the digest list only a single-line
// summary is rendered — otherwise a 50-item digest would become a long document.
//
// All externally sourced content (title/type/assets/summary) goes through markdownText:
// single-lining + escaping. The back-link is assembled from the admin-configured
// public_base_url, is not untrusted content, and must be a clickable link, so it is output
// verbatim.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Digest mode: presented on a single line, with compressed assets and summary following.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**Status change**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**Type**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**Assets**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**Summary**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[View details](%s)\n", it.DetailURL)
	}
}

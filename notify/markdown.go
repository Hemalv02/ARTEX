package notify

import (
	"fmt"
	"strings"
)

// This file contains the message rendering shared by the "Markdown family"
// of channels (DingTalk, WeCom). Feishu uses card JSON, Telegram uses HTML,
// and email uses HTML; each renders in its own adapter.

// maxAssetsShown is the maximum number of assets to list in a message. A
// finding can anchor dozens of assets, and listing them all would
// overflow the message and carry no information value — no one will look
// at the 4th and later domain names in an IM message.
const maxAssetsShown = 3

// maxSummaryRunes is how many characters the summary is compressed to.
// An IM message is a "prompt to go look at the details", not the report
// itself; the full content lives on the platform.
const maxSummaryRunes = 120

// markdownReservedBytes is reserved for the message header (summary line
// + severity distribution + possible truncation notice) and footer
// (platform link). When packing the whole message, this portion is
// deducted from the budget so the header and footer are not truncated —
// if they were, the reader would not even be able to tell "which batch
// is this, how many more are not shown".
const markdownReservedBytes = 320

// markdownEscape escapes markdown metacharacters.
//
// Why it is necessary: vulnerability titles, summaries, types, and
// asset display names all come from **untrusted sources** — the title
// and summary come from model output (the model reads the response of
// the target under test), and the asset url is the full URL obtained
// from the scan (including a target-controllable query string). Without
// escaping, a finding with the title
//
//	Login SQL injection\n[Urgent: verify account here](http://attacker.tld)
//
// would render as a **clickable external link** in a security engineer's
// DingTalk/Feishu; and `![](http://attacker.tld/beacon)` would be
// fetched by the client when rendered, equivalent to notifying that
// "this finding has been seen" and leaking the reader's IP. Even with
// non-malicious content, injected bold or blockquotes can push the
// truly severe findings below the fold.
//
// The escape set covers characters that change structure or produce
// clickable elements: title/link/emphasis/list/quote/strikethrough.
// `\` must be processed first, otherwise the backslashes added later
// would themselves be escaped.
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

// markdownText compresses untrusted text into a single line and escapes
// it, for use in the markdown body. Single-line-ization is the other
// half of the defense: a newline itself can forge new list items or
// blockquotes, and escape characters cannot stop that.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (the IM platform's title bar
// or card title), the content is **unescaped original text**.
//
// This is deliberately not escaped: this title is shared by the
// renderers in four contexts — markdown body, Telegram's HTML, Feishu
// card's plain_text, and the generic Webhook's JSON, as well as the
// email subject. Each context has its own escaping rules (escaping for
// markdown inserted into HTML leaves visible backslashes; into JSON
// pollutes the data), so escaping must be done by the respective output
// side, see writeItem / feishuItemLines / telegramEscape. Markdown
// escaping was once added in the shared function, with the result that
// `\(1\)` appeared visibly in Telegram messages.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Finding summary - %d items", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Vulnerability notification"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body, returning the body and the
// **actual number of items written**.
//
// The kept return value is the number of items actually delivered in
// this delivery; the caller uses this to mark only the first kept
// items as delivered — items that were blocked by the channel length
// limit must be left for the next batch, not marked as successful
// alongside them. This is the source of "silent loss": the message was
// truncated, but the delivery record shows everything delivered, and
// nowhere can you see that the second half was never sent.
//
// maxBytes<=0 means no limit.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// Even if a single message is too long, send it anyway
		// (truncated at the end as a fallback): partial information
		// about a finding is better than none.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[View all on platform](%s)\n", m.HomeURL)
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

// markdownBatchIntro renders the opening of a summary message: the time
// window, item count, and severity distribution. With these, the
// recipient can tell whether the batch needs immediate attention without
// clicking into the platform.
//
// items is the **actually included** items, total is the number that
// should have been in the batch. When the two differ it must be made
// explicit "how many more are in the next message" — otherwise the
// reader will think the number in the header is everything, while the
// items that were never sent do not exist anywhere in the interface.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**%d new findings in the last %d minutes**", total, m.WindowMinutes)
	} else {
		fmt.Fprintf(&b, "**%d new findings**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, " (this message shows the first %d, the remaining %d will continue in the next message)", len(items), extra)
	}
	// Show the severity distribution so the reader can immediately see
	// whether there are critical items. Only count the items **actually
	// included in this message**, to ensure that "critical: 3" matches
	// what can be counted below.
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

// writeItem renders a single finding entry.
//
// prefix is used for the index number in summary lists; when single=true
// it renders the full version (including summary and link back); in
// summary lists it only renders a one-line summary — otherwise a
// 50-item summary becomes a long document.
//
// All content from outside (title/type/asset/summary) is passed through
// markdownText: single-line-ized + escaped. The link back is built from
// the admin-configured public_base_url, is not untrusted content, and
// must be a clickable link, so it is output as-is.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s - %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Summary mode: single line, with assets and summary
		// compressed and appended.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " - " + strings.Join(extras, " · ")
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
		fmt.Fprintf(b, "**Asset**: %s\n", markdownText(a, 0))
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

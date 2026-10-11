package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes truncates s to at most max bytes, guaranteeing that
// the result is valid UTF-8 and does not cut through a character.
//
// Why it must cut on character boundaries: WeCom group bot's
// markdown has a hard limit of 4096 **bytes** (not characters), and
// one Chinese character is 3 bytes. Cutting directly by bytes
// splits a Chinese character in half, producing invalid UTF-8 —
// the platform either rejects the whole thing or displays it as
// garbled boxes. The approach here is to first walk back from the
// budget position to the nearest rune start byte (utf8.RuneStart
// identifies continuation bytes 0b10xxxxxx).
//
// max<=0 means no limit. An ellipsis is appended after truncation,
// unless max is too small to fit even the ellipsis.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than the ellipsis: drop the ellipsis and
		// just truncate, to avoid the result exceeding max.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine collapses multi-line text into a single line: folds all
// whitespace, then truncates by character count. Used for IM message
// title rows — summaries often contain newlines, and inserting them
// directly into tables/titles breaks layout. max<=0 means no length
// limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to at most max characters (not bytes),
// appending an ellipsis when truncated. max<=0 means no limit.
//
// The difference from TruncateBytes is the platform's measurement
// convention: WeCom measures in bytes, Telegram measures in
// characters. Using the wrong convention does not error, it just
// makes the message get cut much shorter than expected (1 Chinese
// character = 3 bytes, so cutting 4096 bytes leaves only about 1365
// characters), so both functions must be kept and selected per
// channel.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML truncates an HTML fragment by character count, and
// guarantees that no half tag is produced.
//
// Direct character truncation on HTML produces broken tags like
// `<a href="htt`, and the platform's parser will either error and
// reject the whole message, or swallow subsequent content as an
// attribute value. The approach here is: first truncate by
// characters, then check whether the tail contains an unclosed `<`,
// and if so back up to before it.
//
// It does not attempt tag balancing (e.g. completing </b>): Telegram's
// HTML parser automatically closes unclosed tags, and doing our own
// balancing has to deal with quotes in attributes, comments, and
// self-closing tags — complexity out of proportion to the payoff.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment starting with "<" (the last "<" is
	// not followed by ">"), back up to before it.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// If the tail is a cut-off HTML entity (e.g. `&amp;` cut to
	// `&amp`), it must also be backed up. An entity fragment in a
	// parser that only recognizes entities can cause the **whole
	// message** to be rejected — a summary message exceeding the
	// length limit is already common, so it is not worth losing the
	// whole notification for that.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount computes how many items can fit in the budget as
// **whole items**, for summary messages packed by whole item.
//
// Why pack by whole item rather than rendering the full document
// and then truncating: truncation would make the latter items
// disappear, while their delivery records would still be marked as
// delivered — invisible in the message, invisible in the delivery
// history, the finding just disappears. After packing by whole
// items, the ones that don't fit remain in the database to become
// the next batch, and the kept returned to the caller is the number
// of items actually delivered in this message.
//
// Parameters: maxSize<=0 means no limit; reserve is the amount
// reserved for the message header/footer; size handles measurement
// (each platform has its own convention: WeCom/DingTalk by bytes,
// Telegram by character count — using the wrong convention does not
// error, it just compresses Chinese messages much smaller than the
// limit); render renders the idx-th item into its actual text — the
// length varies by content, so it cannot rely on estimation.
//
// At least 1 is returned (as long as there are items). When a
// single item is extremely long it is still emitted, with the
// caller's final truncation acting as a backstop, otherwise an
// overlong finding would permanently stall the whole batch.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize are the two measurement conventions used by
// packItemCount; named here to avoid naked func(s string) int
// closures at the call site, which would make it hard to see at a
// glance which convention is being used.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders an asset list as a single line of display text,
// omitting the rest with a total count when the limit is exceeded.
// A finding may anchor dozens of assets; listing them all would
// overflow the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " and " + itoa(len(assets)) + " more"
}

// itoa is a short alias for strconv.Itoa, only used for concatenating
// display text, to avoid importing strconv everywhere.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
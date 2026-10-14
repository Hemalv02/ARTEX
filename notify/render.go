package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes truncates s to at most max bytes, guaranteeing the result is valid UTF-8
// and does not cut a character in half.
//
// Why cutting on a character boundary is essential: the WeCom group bot's markdown has a
// hard limit of 4096 **bytes** (not characters), and a Chinese character is 3 bytes.
// Slicing directly by byte would cut a Chinese character in half and produce invalid UTF-8
// — the platform would either reject the whole message or display garbled replacement
// boxes. The approach here is to first step back from the budget position to the nearest
// rune-start byte (utf8.RuneStart identifies continuation bytes 0b10xxxxxx).
//
// max<=0 means no limit. An ellipsis is appended after truncation, unless max is too small
// to fit the ellipsis.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than the ellipsis: drop the ellipsis and truncate plainly, to avoid
		// a result that actually exceeds max.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine compresses multi-line text to a single line: collapse all whitespace, then
// truncate by character count. Used for the title line of IM messages — summaries often
// contain newlines, and dropping them straight into a table/title breaks the layout.
// max<=0 means no length limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to at most max characters (not bytes), appending an ellipsis
// when it overflows. max<=0 means no limit.
//
// The difference from TruncateBytes is the platform's unit of measure: WeCom limits by
// bytes, Telegram limits by character count. Using the wrong unit won't raise an error, it
// will just cut the message far shorter than expected (a Chinese character = 3 bytes, so
// cutting at 4096 bytes leaves only about 1365 characters), so both functions must be kept
// and chosen per channel.
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

// TruncateHTML truncates an HTML fragment by character count while guaranteeing it won't
// produce a half-cut tag.
//
// Truncating HTML directly by character would cut out a broken tag like `<a href="htt`,
// and the platform parser would either error out and reject the whole message or swallow
// the following body as an attribute value. The approach here is: first truncate by
// character, then check whether the tail has an unclosed `<`, and if so step back to
// before it.
//
// Tag balancing (completing a </b> and the like) is not done: Telegram's HTML parser auto-
// closes unclosed tags, and implementing balancing ourselves would require handling quotes
// in attributes, comments, and self-closing tags — complexity out of proportion to the benefit.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment starting with `<` (no `>` after the last `<`), step back to
	// before the `<`.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// If the tail is a cut-off HTML entity (e.g. `&amp;` cut to `&amp`), step back as well.
	// An entity fragment can cause a parser that only accepts entities to reject the
	// **whole message** — a digest message exceeding the length limit is common enough on
	// its own, and it's not worth losing the entire notification over.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount computes how many items fit **completely** within the budget, for packing
// a digest message item by item.
//
// Why pack by whole item rather than render the full document and then truncate:
// truncation makes the latter half of the items vanish while their delivery records are
// still marked as delivered — invisible in the message, invisible in the delivery history,
// and the findings are just gone. After packing by whole item, items that don't fit stay
// in the database as the next batch, and the kept the caller receives is the number of
// items genuinely delivered in this message.
//
// Parameters: maxSize<=0 means no limit; reserve is the amount set aside for the message
// header/footer; size does the measuring (the unit differs per platform: WeCom/DingTalk by
// bytes, Telegram by character count — using the wrong unit won't error, it just compresses
// a Chinese message to far below the limit); render turns the item at idx into its actual
// text — length varies with content and can't be estimated.
//
// Returns at least 1 (as long as there are any items). An extremely overlong single item
// is still sent, backstopped by the caller's final truncation; otherwise one overlong
// finding would wedge the whole batch in place permanently.
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

// byteSize / runeSize are packItemCount's two measuring units, named out to avoid a bare
// func(s string) int closure at the call site, which would make it hard to tell at a
// glance which unit is in use.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders the asset list into a single line of display text, omitting the rest
// and noting the total when there are more than limit. A single finding may be anchored to
// dozens of assets, and listing them all would overwhelm the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " and " + itoa(len(assets)) + " total"
}

// itoa is a short alias for strconv.Itoa, used only for assembling display text, to avoid
// importing strconv everywhere.
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

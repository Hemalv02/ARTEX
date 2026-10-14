package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// This is the single most important invariant in this package. WeCom limits by **bytes**, and Chinese is 3 bytes/character,
	// so any implementation that hard-cuts by byte would slice a character in half, producing invalid UTF-8 that the platform rejects.
	// Use several Chinese/English mixed inputs of coprime lengths to hit every possible cut point.
	inputs := []string{
		"中文测试内容",
		"混合 mixed 内容 content",
		"a中b文c测d试e",
		"🔴🟠🟡🔵", // 4-byte emoji, a bad cut shows up more clearly
		strings.Repeat("漏洞", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("input %q max=%d: produced invalid UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("input %q max=%d: result %d bytes exceeds the limit", in, max, len(got))
			}
			// When not truncated, the content must not be changed.
			if len(in) <= max && got != in {
				t.Fatalf("input %q max=%d: changed the content despite being under the limit -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 should mean unlimited")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 should mean unlimited")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// When max is smaller than the ellipsis itself, appending the ellipsis must not push it over the limit.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("at max=1 the result %q has length %d, over the limit", got, len(got))
	}
	// In the normal case it should carry the ellipsis.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("expected an ellipsis, got %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// The difference in measure from TruncateBytes must be preserved: Telegram limits by character,
	// and using a byte measure would cut a Chinese message down to a third.
	s := "一二三四五六七八九十"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("expected 5 characters, got %d (%q)", n, got)
	}
	// The same string under the byte measure should be noticeably shorter.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("the byte measure should not produce the same character count as the character measure")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("第一行\n\n第二行\t带制表   多空格", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("should collapse all whitespace, got %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("should not keep consecutive spaces, got %q", got)
	}
	// After truncation it must still be readable and valid.
	got = OneLine("一二三四五六七八九十", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("expected 4 characters, got %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// Truncating HTML directly would cut out fragments like `<a href="htt`, and the platform rejects the whole message.
	s := `<b>标题</b>正文正文正文<a href="https://example.com/very/long/path">查看详情</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: result %d characters over the limit", max, n)
		}
		// The tail must not have an unclosed `<` (i.e. a `<` with no `>` in the last segment).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: tail tag was cut off -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("no assets should return an empty string, got %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("under the limit all should be listed, got %q", got)
	}
	// When over the limit it must note the total, or readers do not know how many assets are not listed.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "5 total") {
		t.Fatalf("should note the total of 5, got %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("an empty severity has ordinal 0 and should be blocked by any threshold")
	}
	if !AtLeast("critical", "") {
		t.Fatal("an empty threshold should let it through")
	}
	if got := StatusLabel("fixed"); got != "Fixed" {
		t.Fatalf("status mapping is wrong, got %q", got)
	}
	// Unknown statuses are echoed verbatim, with no fabricated label.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("an unknown status should be echoed verbatim, got %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity covers a gap the audit pointed out: truncation must avoid not only
// half-cut tags but also cut-off HTML entities.
//
// After `&amp;` is cut to `&amp`, a parser that only recognizes entities may reject the **whole** message —
// and overlong digest messages are common to begin with, so the cost is too high.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// The tail must not contain an entity fragment with "a & but no matching ;".
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: left an entity fragment at the tail %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: malformed entity appeared", max)
		}
	}
}

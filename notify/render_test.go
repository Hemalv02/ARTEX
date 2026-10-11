package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// This is the most important invariant in this package. WeCom
	// measures in **bytes**, Chinese is 3 bytes/character, and any
	// implementation that hard-cuts by bytes will cut a Chinese
	// character in half, producing invalid UTF-8 and getting
	// rejected by the platform. Use a variety of mixed Chinese /
	// English inputs with coprime lengths to hit every possible
	// cut point.
	inputs := []string{
		"Chinese test content",
		"mixed mixed content content",
		"a中b文c测d试e",
		"🔴🟠🟡🔵", // 4-byte emoji, even more obvious when cut wrong
		strings.Repeat("Finding", 100),
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
			// Content must not be modified when not truncated.
			if len(in) <= max && got != in {
				t.Fatalf("input %q max=%d: content was modified when not over limit -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 should mean no limit")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 should mean no limit")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// When max is smaller than the ellipsis itself, the result
	// must not exceed max by appending the ellipsis.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1 result %q has length %d, exceeds the limit", got, len(got))
	}
	// In the normal case it should carry the ellipsis.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("expected with ellipsis, got %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// The convention difference with TruncateBytes must be
	// preserved: Telegram measures in characters; the byte
	// convention would cut Chinese messages down to a third.
	s := "OneTwoThreeFourFiveSixSevenEightNineTen"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("expected 5 characters, got %d (%q)", n, got)
	}
	// The same string should be noticeably shorter under the byte
	// convention.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("byte convention should not produce the same character count as the character convention")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("line one\n\nline two\twith tab   multi space", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("should fold all whitespace, got %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("consecutive spaces should not be preserved, got %q", got)
	}
	// After truncation it must still be readable and valid.
	got = OneLine("OneTwoThreeFourFiveSixSevenEightNineTen", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("expected 4 characters, got %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// Direct truncation of HTML will cut a fragment like
	// `<a href="htt`, and the platform will reject the whole
	// message.
	s := `<b>Title</b>body body body<a href="https://example.com/very/long/path">View details</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: result %d characters exceeds the limit", max, n)
		}
		// The tail must not have an unclosed `<` (i.e. a `<`
		// appears in the last segment but no matching `>`).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: tail tag is cut -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("no assets should return an empty string, got %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("when under the limit, all should be listed, got %q", got)
	}
	// When over the limit, the total count must be marked,
	// otherwise the reader does not know how many more assets
	// were left out.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "and 5 more") {
		t.Fatalf("should mark the total as 5, got %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("empty severity's ordinal is 0 and should be blocked by any threshold")
	}
	if !AtLeast("critical", "") {
		t.Fatal("empty threshold should let everything through")
	}
	if got := StatusLabel("fixed"); got != "Fixed" {
		t.Fatalf("unknown status mapping, got %q", got)
	}
	// Unknown statuses are echoed back as-is, no fabricated label.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("unknown status should be echoed as-is, got %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity covers a gap the audit pointed
// out: truncation must not only avoid half-cut tags, but also
// HTML entities that are cut off.
//
// When `&amp;` is cut into `&amp`, a parser that only recognizes
// entities may reject the **whole** message — and overlong summary
// messages are common, so the cost is too high.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// The tail must not contain an entity fragment
		// "has & but no matching ;".
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: tail leaves an entity fragment %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: malformed entity appeared", max)
		}
	}
}

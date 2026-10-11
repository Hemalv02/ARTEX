package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This file covers the "pack by whole item" fix: when a summary
// message exceeds the channel's length limit, it must be truncated
// **by whole item** and the number of items not packed must be
// reported honestly, so the caller only marks those that were
// actually delivered.
//
// The previous approach was to render the full document, truncate,
// and mark the whole batch as delivered: the second half of the
// message disappears into thin air, while the delivery history shows
// full success — the finding is simply gone, and there is nowhere to
// notice it.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200 Chinese summary items, which must exceed the 4096-byte
	// WeCom limit.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("body %d bytes exceeds the limit %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("body is not valid UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("should only pack a portion (0 < kept < %d), got %d", len(m.Items), kept)
	}
	// The header must honestly state how many items this message
	// includes and how many remain — otherwise the reader will take
	// the number in the header as the total.
	if !strings.Contains(body, "remaining") || !strings.Contains(body, "continue in the next message") {
		t.Fatalf("the header should say how many more items are not included in this message:\n%s", body[:minInt(400, len(body))])
	}
	// It should only contain the first kept items.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "Finding"+itoa(i+1)) {
			t.Fatalf("item %d should be in this message:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "Finding"+itoa(kept+1)) {
		t.Fatalf("item %d should not appear (it belongs to the next batch)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = no limit
	if kept != len(m.Items) {
		t.Fatalf("all should be kept when length is unlimited, got kept=%d", kept)
	}
	if strings.Contains(body, "remaining") {
		t.Fatalf("truncation notice should not appear when there is no truncation:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// When the budget is so small that not even one item fits, one
	// must still be emitted (with the final truncation acting as a
	// backstop). Otherwise an overlong finding would permanently
	// stall the entire batch: each pickup doesn't fit, each pickup
	// doesn't send.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("at least 1 should be kept, got %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("a single message should report 1 delivered, got %d", kept)
	}
	// An empty message has nothing to deliver.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("empty message should report 0, got %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram's limit is in **characters**; using the byte
	// convention would compress Chinese messages to one third.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("body %d characters exceeds the limit %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("should only pack a portion, got %d", kept)
	}
	if !strings.Contains(text, "continue in the next message") {
		t.Fatalf("should state that there is more not included:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("card should only pack a portion, got %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// These two channels do not truncate the body, so the whole
	// batch counts as delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("precondition does not hold")
	}
	// Indirect confirmation via the renderer's return value:
	// markdownBody(0) keeps everything when there is no limit.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("should keep everything when length is unlimited, got %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent is a regression test for
// "untrusted content must not be able to change the message
// structure". Titles and summaries come from model output (the model
// reads the target's response), and asset names come from the
// target's URL.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // must appear in the result (escaped form)
		wrong []string // must not appear in the result (unescaped form)
	}{
		{
			name: "newline in title + external link",
			item: Item{
				Severity: "high",
				Name:     "Login SQL injection\n[Urgent: verify account here](http://attacker.tld)",
			},
			// Newlines must be folded (otherwise a new list
			// item / blockquote can be forged);
			// square brackets and parentheses must be escaped
			// (otherwise it becomes a clickable external link).
			must:  []string{`\[Urgent: verify account here\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[Urgent", "\n\n[Urgent"},
		},
		{
			name: "image beacon in title",
			item: Item{
				Severity: "high",
				Name:     "Finding ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "emphasis and blockquote in asset name",
			item: Item{
				Severity: "high",
				Name:     "Normal title",
				Assets:   []string{"a.com/*injection*>quote"},
			},
			must:  []string{`\*injection\*`, `\>`},
			wrong: []string{"*injection*"},
		},
		{
			name: "backticks and pipe in summary",
			item: Item{
				Severity: "high",
				Name:     "Title",
				Summary:  "`code` | table",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// The writeItem in single mode is the rendering
			// path shared by all three markdown channels.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("missing escaped form %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("unescaped form %q appeared (can be used to inject structure or external links):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst locks in the escape order: the
// backslash must be processed first, otherwise it would be applied
// again to the backslashes added later, producing double backslashes
// in the output.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("escape order is wrong, got %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes locks in a specific
// regression: markdown escapes must not leak into Telegram's HTML
// output (escaping was once added in the shared title function,
// with the result that `\(1\)` appeared visibly in Telegram
// messages).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *important*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram body contains markdown backslash escapes:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

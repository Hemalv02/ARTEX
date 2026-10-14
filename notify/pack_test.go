package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This file covers the "pack by whole item" fix: when a digest message exceeds the channel's length limit, it must truncate
// **by whole item** and faithfully report how many items did not fit, so the caller only marks the ones actually delivered.
//
// The previous approach rendered the full text, then truncated, then marked the whole batch delivered: the second half of the message
// vanished while delivery history showed full success — findings just disappeared, with nowhere to detect it.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200 Chinese digest items, far exceeding WeCom's 4096 bytes for sure.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("body is %d bytes, over the limit of %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("body is not valid UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("should fit only a portion (0 < kept < %d), got %d", len(m.Items), kept)
	}
	// The header must faithfully state how many items this message contains and how many remain — otherwise readers take the header
	// number as the total.
	if !strings.Contains(body, "the remaining") || !strings.Contains(body, "continue in the next message") {
		t.Fatalf("the header should state how many items are not included in this message:\n%s", body[:minInt(400, len(body))])
	}
	// Should include only the first kept items.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "漏洞"+itoa(i+1)) {
			t.Fatalf("item %d should be in this message:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "漏洞"+itoa(kept+1)) {
		t.Fatalf("item %d should not appear (it belongs to the next batch)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = no limit
	if kept != len(m.Items) {
		t.Fatalf("with no length limit all should be kept, got kept=%d", kept)
	}
	if strings.Contains(body, "其余") {
		t.Fatalf("no truncation notice should appear when there is no truncation:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// When the budget is too small to fit even one item, one must still be emitted (the final truncation is the fallback).
	// Otherwise a single overlong finding would jam the whole batch permanently: every claim fails to fit and nothing is ever sent.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("at least 1 item should be kept, got %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("a single-item message should report 1 delivered, got %d", kept)
	}
	// An empty message has no deliverable items.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("an empty message should report 0 items, got %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram limits by **character count**; using a byte measure would squash a Chinese message to a third.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("body is %d characters, over the limit of %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("should fit only a portion, got %d", kept)
	}
	if !strings.Contains(text, "continue in the next message") {
		t.Fatalf("should state that there is remaining content not included:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("the card should fit only a portion, got %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// These two channels do not truncate the body; the whole batch counts as delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("precondition does not hold")
	}
	// Indirectly confirm via the renderer's return value: markdownBody(0) keeps everything when unlimited.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("with no length limit all should be used, got %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent is a regression test for "untrusted content must not change the message structure".
// The title and summary come from model output (the model reads the target-under-test response); the asset name comes from the target-under-test URL.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // must appear in the result (escaped form)
		wrong []string // must not appear in the result (unescaped form)
	}{
		{
			name: "newline + external link in the title",
			item: Item{
				Severity: "high",
				Name:     "登录口 SQL 注入\n[紧急：点此验证账号](http://attacker.tld)",
			},
			// Newlines must be collapsed (otherwise new list items/blockquotes can be forged);
			// brackets and parentheses must be escaped (otherwise it becomes a clickable external link).
			must:  []string{`\[紧急：点此验证账号\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[紧急", "\n\n[紧急"},
		},
		{
			name: "image beacon in the title",
			item: Item{
				Severity: "high",
				Name:     "漏洞 ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "emphasis and blockquote in the asset name",
			item: Item{
				Severity: "high",
				Name:     "普通标题",
				Assets:   []string{"a.com/*注入*>引用"},
			},
			must:  []string{`\*注入\*`, `\>`},
			wrong: []string{"*注入*"},
		},
		{
			name: "backtick and pipe in the summary",
			item: Item{
				Severity: "high",
				Name:     "标题",
				Summary:  "`code` | 表格",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// writeItem in single-item mode is the rendering path shared by the three markdown channels.
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
					t.Errorf("unescaped form %q appeared (could be used to inject structure or an external link):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst locks down the escape order: the backslash must be processed first,
// otherwise it wraps another layer around the backslashes added later and double backslashes appear in the output.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("escape order is wrong, got %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes locks down a specific regression:
// markdown escaping must not leak into Telegram's HTML output (escaping was once added in the shared title function,
// so visible backslashes like `\(1\)` appeared in Telegram messages).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *重点*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("markdown backslash escapes appeared in the Telegram body:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

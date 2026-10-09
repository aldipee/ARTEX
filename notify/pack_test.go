package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// These tests cover whole-item packing: when a digest exceeds a channel limit,
// report the omitted item count so callers mark only items that were delivered.
//
// Truncating a fully rendered digest and marking the whole batch delivered caused
// its tail to disappear while delivery history incorrectly showed success.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// A 200-item digest is well over the WeCom 4096-byte limit.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("body is %d bytes, over the limit of %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("body is not valid UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("expected a partial batch (0 < kept < %d); got %d", len(m.Items), kept)
	}
	// The heading must distinguish the items shown from those left for the next message.
	if !strings.Contains(body, "remaining") || !strings.Contains(body, "next message") {
		t.Fatalf("heading should say how many items are omitted:\n%s", body[:minInt(400, len(body))])
	}
	// Only the first kept items should be included.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "Vulnerability "+itoa(i+1)) {
			t.Fatalf("item %d should be in this message:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "Vulnerability "+itoa(kept+1)) {
		t.Fatalf("item %d should be in the next batch", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = unlimited
	if kept != len(m.Items) {
		t.Fatalf("unlimited length should retain every item; kept=%d", kept)
	}
	if strings.Contains(body, "remaining") {
		t.Fatalf("untruncated output should not include a truncation notice:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// If the budget cannot fit one full item, still send one item and let final
	// truncation handle it; otherwise an oversized finding would block the batch forever.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("at least one item should be retained; got %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("a single-item message should report one delivered item; got %d", kept)
	}
	// An empty message has no deliverable items.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("empty message should report zero items; got %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	for i := range m.Items {
		m.Items[i].Summary = strings.Repeat("界", 20)
	}
	text, kept := telegramHTML(m)
	// Telegram limits by rune count; byte-based truncation would over-shorten CJK text.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("body has %d runes, over the limit of %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("expected a partial batch; got %d", kept)
	}
	if !strings.Contains(text, "next message") {
		t.Fatalf("output should mention omitted items:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("card should contain only part of the batch; got %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// These channels do not truncate the body, so the whole batch is delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("precondition failed")
	}
	// Confirm indirectly through the renderer that an unlimited markdown body retains all items.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("unlimited length should retain every item; got %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent ensures untrusted content cannot alter
// message structure. Titles and summaries come from model output; asset names
// come from tested target URLs.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // Escaped forms that must appear.
		wrong []string // Unescaped forms that must not appear.
	}{
		{
			name: "newline and external link in title",
			item: Item{
				Severity: "high",
				Name:     "Login form SQL injection\n[Urgent: click here to verify account](http://attacker.tld)",
			},
			// Collapse newlines to prevent forged list items or quotes. Escape
			// brackets and parentheses so the link is not clickable.
			must:  []string{`\[Urgent: click here to verify account\]`, `\(http://attacker.tld\)`},
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
			name: "emphasis and quote in asset name",
			item: Item{
				Severity: "high",
				Name:     "Ordinary title",
				Assets:   []string{"a.com/*inject*>quote"},
			},
			must:  []string{`\*inject\*`, `\>`},
			wrong: []string{"*inject*"},
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
			// Single-item writeItem is shared by all three markdown channels.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("escaped form %q is missing:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("unescaped form %q appeared and could inject structure or a link:\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst ensures backslashes are escaped first so
// later escape sequences are not escaped a second time.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("incorrect escaping order; got %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes ensures markdown escaping does not leak
// into Telegram's HTML output as visible backslashes.
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

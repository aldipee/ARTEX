package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// WeCom limits content by **bytes**. Cutting a multibyte rune at an arbitrary
	// byte boundary produces invalid UTF-8, so exercise different UTF-8 boundaries.
	inputs := []string{
		strings.Repeat("界", 5),
		"mixed " + strings.Repeat("界", 3) + " content",
		"a" + strings.Repeat("界", 4) + "e",
		"🔴🟠🟡🔵", // Four-byte emoji make incorrect cuts easier to detect.
		strings.Repeat("界", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("input %q max=%d: produced invalid UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("input %q max=%d: result is %d bytes, over the limit", in, max, len(got))
			}
			// Do not modify content when it is not truncated.
			if len(in) <= max && got != in {
				t.Fatalf("input %q max=%d: content changed even though it fit -> %q", in, max, got)
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
	// Adding an ellipsis must not exceed the limit when max is smaller than the ellipsis.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("result %q has length %d, exceeding max=1", got, len(got))
	}
	// A normal truncation should include an ellipsis.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("expected an ellipsis; got %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// Telegram limits by rune count, unlike TruncateBytes.
	s := strings.Repeat("界", 10)
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("expected 5 runes; got %d (%q)", n, got)
	}
	// The same input should be much shorter when truncated by bytes.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("byte-based truncation should not retain the same rune count")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("First line\n\nSecond line\twith tabs   and spaces", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("all whitespace should be collapsed; got %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("consecutive spaces should not remain; got %q", got)
	}
	// Truncated content should remain readable and valid.
	got = OneLine(strings.Repeat("界", 10), 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("expected 4 runes; got %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// Naive HTML truncation can leave a partial tag such as `<a href="htt`.
	s := `<b>Title</b>Body text<a href="https://example.com/very/long/path">View details</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: result has %d runes, over the limit", max, n)
		}
		// The final segment must not contain an unclosed '<' tag.
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: trailing tag was cut off -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("empty assets should return an empty string; got %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("all assets should be listed when within the limit; got %q", got)
	}
	// When the list is shortened, include the total so readers know how many assets are omitted.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "and 5 more") {
		t.Fatalf("expected the total of 5 to be shown; got %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("empty severity has rank 0 and should be blocked by any threshold")
	}
	if !AtLeast("critical", "") {
		t.Fatal("an empty threshold should allow the event")
	}
	if got := StatusLabel("fixed"); got != "Fixed" {
		t.Fatalf("unexpected status label: %q", got)
	}
	// Preserve unknown statuses verbatim instead of inventing a label.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("unknown status should be preserved verbatim; got %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity ensures truncation avoids cutting HTML entities
// as well as tags.
//
// If `&amp;` is cut into `&amp`, a parser that requires complete entities may
// reject the **entire** message, which is costly for common long digests.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// The end must not contain an entity fragment with '&' but no matching ';'.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: trailing entity fragment remains: %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: malformed entity found", max)
		}
	}
}

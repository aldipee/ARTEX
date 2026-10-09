package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes truncates s to at most max bytes, guaranteeing valid UTF-8 and no split characters.
//
// Why it must cut on character boundaries: the WeCom group bot's markdown has a 4096-**byte** hard cap (not character
// count), and one Chinese character is 3 bytes. Slicing directly on bytes would split a character in half and produce
// invalid UTF-8 — the platform would either reject the whole message or render it as mojibake boxes. The approach here
// is to walk back from the budget position to the nearest rune start byte (utf8.RuneStart identifies continuation
// bytes 0b10xxxxxx).
//
// max<=0 means no limit. An ellipsis is appended after truncation, unless max is too small to fit it.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than the ellipsis itself: drop the ellipsis and do a pure truncation, avoiding a result that
		// exceeds max.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine collapses multi-line text to a single line: folds all whitespace, then truncates by character count.
// Used for IM message title lines — summaries often contain newlines, and stuffing them into a table/title would break
// the layout. max<=0 means no length limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to at most max characters (not bytes), appending an ellipsis when exceeded.
// max<=0 means no limit.
//
// The difference from TruncateBytes is the platform's measuring convention: WeCom limits by bytes, Telegram limits by
// character count. Using the wrong convention won't error, it just truncates far more than expected (one Chinese char
// = 3 bytes, so cutting 4096 bytes leaves only ~1365 chars), so both functions must be kept and chosen per channel.
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

// TruncateHTML truncates an HTML fragment by character count while guaranteeing no half-open tags.
//
// Truncating HTML directly by characters can cut out a broken tag like `<a href="htt`; the platform parser would
// either reject the whole message or swallow the following body as an attribute value. The approach here: truncate by
// characters first, then check whether the tail has an unclosed `<` and back off to before it if so.
//
// No tag balancing (closing </b> etc.): Telegram's HTML parser auto-closes unclosed tags, and implementing balancing
// ourselves would have to handle quotes in attributes, comments, and self-closing tags — complexity out of proportion
// to the benefit.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment starting with `<` (no `>` after the last `<`), back off to before the `<`.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// If the tail is a cut-off HTML entity (e.g. `&amp;` cut to `&amp`), also back off.
	// An entity fragment can get the **whole message** rejected by an entity-only parser — an over-length digest
	// message is already common, not worth losing the whole notification for it.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount computes how many items fit **whole** within the budget, for packing digest messages per whole item.
//
// Why pack whole items rather than render the full body and truncate: truncation makes the tail items vanish, yet
// their delivery records would still be marked delivered — invisible in the message and in the delivery history alike,
// the vulnerability just disappears. After packing whole items, the items that didn't fit stay in the DB as the next
// batch, and the kept count returned to the caller is the number of items actually delivered in this message.
//
// Params: maxSize<=0 means no limit; reserve is the amount reserved for the message header/footer; size is the
// measuring function (each platform's convention differs: WeCom/DingTalk by bytes, Telegram by character count — using
// the wrong convention won't error, it just compresses Chinese messages to far below the limit); render renders the
// idx-th item into its actual text — length varies by content and can't be estimated.
//
// Returns at least 1 (as long as there are items). A single extremely long item must still be sent, with the caller's
// final truncation as the fallback, otherwise one over-long vulnerability would permanently stall the whole batch.
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

// byteSize / runeSize are the two measuring conventions for packItemCount, named to avoid bare
// func(s string) int closures at call sites, which would make it hard to see at a glance which convention is in use.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders an asset list into a one-line display string, eliding the rest and annotating the total when
// exceeding limit. A vulnerability may anchor dozens of assets; listing them all would burst the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " and " + itoa(len(assets)) + " more"
}

// itoa is a short alias for strconv.Itoa, used only for assembling display text to avoid importing strconv everywhere.
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

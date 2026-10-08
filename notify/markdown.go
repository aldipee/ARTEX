package notify

import (
	"fmt"
	"strings"
)

// This file renders messages shared by the "Markdown-family" channels (DingTalk,
// WeCom). Feishu uses card JSON, Telegram uses HTML, and email uses HTML, each
// rendered in its own adapter.

// maxAssetsShown is the maximum number of assets listed in a message. A vulnerability
// can be anchored to dozens of assets; listing them all would crowd the message with
// no informational value — nobody reads domains past the 4th in an IM.
const maxAssetsShown = 3

// maxSummaryRunes is the maximum character count the summary is compressed to. An IM
// message is a "hint to view details", not the report itself; the full content lives
// in the platform.
const maxSummaryRunes = 120

// markdownReservedBytes is reserved for the message head (digest line + severity
// distribution + a possible truncation note) and tail (the platform link). When
// packing whole items this is subtracted from the budget so the head and tail are not
// truncated — once they are, the reader can't tell "which batch this is, or how many
// items aren't shown".
const markdownReservedBytes = 320

// markdownEscape escapes markdown metacharacters.
//
// Why it's necessary: the vulnerability title, summary, class, and asset display names
// all come from **untrusted sources** — the title and summary come from model output
// (the model reads the target's response), and the asset url is the full scanned URL
// (including a target-controllable query string). Without escaping, a vulnerability
// titled
//
//	login SQL injection\n[URGENT: click to verify account](http://attacker.tld)
//
// would render in a security engineer's DingTalk/Feishu as a **clickable external
// link**; and `![](http://attacker.tld/beacon)` would be fetched by the client at
// render time, broadcasting "this vulnerability has been viewed" and leaking the
// reader's IP. Even with benign content, injected bold or blockquote markup can push
// the critical vulnerabilities below out of the fold.
//
// The escape set covers characters that change structure or create clickable elements
// in titles/links/emphasis/lists/quotes/strikethrough. `\` must be handled first,
// otherwise the backslashes added later would be escaped again.
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

// markdownText flattens untrusted text to a single line and escapes it, for use in
// markdown bodies. Flattening is the other half beyond escaping: a newline itself can
// forge a new list item or blockquote, and escaping characters doesn't stop that.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (the title bar / card title on IM
// platforms), as **unescaped original text**.
//
// Escaping is deliberately not done here: this title is shared by four rendering
// contexts — markdown body, Telegram HTML, Feishu card plain_text, and generic
// webhook JSON plus email subject. Each context has different escaping rules (markdown
// escaping dropped into HTML leaves visible backslashes; dropped into JSON it corrupts
// data), so escaping must be handled by each output side, see writeItem /
// feishuItemLines / telegramEscape. Markdown escaping was once added to a shared
// function, and the result was visible backslashes like `\(1\)` in Telegram messages.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Vulnerability Digest · %d total", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Vulnerability Notification"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body and returns the body plus the number of items
// **actually written**.
//
// The returned kept is the number of items truly delivered by this send; the caller
// marks only the first kept items delivered — items cut off by the channel length limit
// must stay for the next batch rather than being marked successful alongside the rest.
// This is exactly the source of "silent loss": the message is truncated, but the
// delivery record shows everything delivered, and nothing reveals that the tail was
// never sent.
//
// maxBytes<=0 means no limit.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// A single message is sent even if it exceeds the limit (the final truncation
		// is the backstop): partial information about one vulnerability beats sending
		// none at all.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[View all on the platform](%s)\n", m.HomeURL)
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

// markdownBatchIntro renders the digest opening: time window, count, and severity
// distribution. With these, the recipient can judge whether this batch needs immediate
// handling without opening the platform.
//
// items are the items **actually included**, total is the full count for this batch.
// When they differ it must state explicitly "how many more are in the next message" —
// otherwise the reader assumes the number in the header is everything, while the
// never-sent items don't exist anywhere in the UI.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**In the last %d minutes, %d new vulnerabilities**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**%d new vulnerabilities**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, " (showing the first %d, the remaining %d continue in the next message)", len(items), extra)
	}
	// Give the distribution by level so the reader can see at a glance whether there
	// are critical items. Only the items **actually included** are counted, so that
	// "Critical 3" matches the items that can be counted below.
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

// writeItem renders a single vulnerability item.
//
// prefix is the ordinal for the digest list; single=true renders the full version
// (with summary and link back), while the digest list renders only a one-line summary —
// otherwise 50 digest items would become a long document.
//
// All externally-sourced content (title/class/assets/summary) goes through
// markdownText: flattening + escaping. The link back is built from the admin-configured
// public_base_url, is not untrusted content, and must be a clickable link, so it is
// output as-is.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Digest mode: single line, with assets and a compressed summary trailing.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
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
		fmt.Fprintf(b, "**Assets**: %s\n", markdownText(a, 0))
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

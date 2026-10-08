// Package notify implements the IM / email push channel adapter layer for
// vulnerability findings.
//
// Layering: this package is a **leaf package** that depends only on the standard
// library. It knows nothing about the database or the server. Channel configs arrive
// as map[string]any (corresponding to the notification_channels.config JSONB column),
// and the content to push arrives as a Message. Splitting it this way means the parts
// most likely to go wrong — signature computation, UTF-8 truncation, filter matching —
// can be unit-tested apart from PostgreSQL, while the host only needs to orchestrate
// them on the server side.
//
// Concurrency contract: Channel implementations must be **stateless**. A single
// Channel instance is reused concurrently across multiple channel configs (and even
// multiple bot instances of the same channel); all credentials are passed in via the
// cfg argument, and things like webhook URLs must never be cached into the
// implementation's own fields.
package notify

// Channel type identifiers. The values are also the valid set for
// notification_channels.kind, validated against a server-side allowlist (like
// findings.status, avoiding a DB CHECK so new channels are easy to add later).
const (
	KindDingTalk = "dingtalk" // DingTalk custom bot
	KindFeishu   = "feishu"   // Feishu (incl. Lark) custom bot
	KindWeCom    = "wecom"    // WeCom group bot
	KindWebhook  = "webhook"  // generic Webhook: custom method/headers/JSON template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP email
)

// Event types, corresponding to notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the fallback for an empty kind in config.
const InitKind = KindDingTalk

// severityRank maps vulnerability levels to comparable ordinals. An unknown level
// returns 0, so any min_severity setting blocks unknown levels — when in doubt, don't
// push, to avoid flooding with false positives.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank returns the ordinal of a level; unknown levels return 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns the emoji-prefixed English level name, used for message titles
// and card colors. Unknown levels are echoed back verbatim rather than invented.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 Critical"
	case "high":
		return "🟠 High"
	case "medium":
		return "🟡 Medium"
	case "low":
		return "🔵 Low"
	default:
		return severity
	}
}

// StatusLabel translates a handling status into English, for status-change messages.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "Pending"
	case "in_progress":
		return "In Progress"
	case "confirmed":
		return "Confirmed"
	case "resolved":
		return "Resolved"
	case "fixed":
		return "Fixed"
	case "false_positive":
		return "False Positive"
	case "ignored":
		return "Ignored"
	case "duplicate":
		return "Duplicate"
	case "risk_accepted":
		return "Risk Accepted"
	default:
		return status
	}
}

// AtLeast reports whether severity meets the min threshold. An empty min means no
// threshold and always passes. Note that an unknown severity has ordinal 0 and is
// rejected by any non-empty min (see the severityRank comment).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}

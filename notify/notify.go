// Package notify implements the IM / email push channel adapter
// layer for vulnerability findings.
//
// Layering: this package is a **leaf package** and only depends on
// the standard library. It does not know about the database or the
// server. Channel config is passed in as map[string]any (corresponding
// to the JSONB column notification_channels.config), and the content
// to be pushed is passed in as Message. The benefit of this split is
// that the truly error-prone parts — signature calculation, UTF-8
// truncation, filter matching — can be unit-tested independently of
// PostgreSQL, with the host only doing orchestration on the server
// side.
//
// Concurrency convention: Channel implementations must be
// **stateless**. The same Channel instance is reused concurrently
// across multiple channel configurations (even multiple bot
// instances of the same channel); all credentials are passed in via
// the cfg parameter, and it is not allowed to cache things like the
// webhook URL into the implementation's own fields.
package notify

// Channel type identifiers. The values are also the legal set for
// notification_channels.kind, validated by a whitelist on the
// server side (same as findings.status, no DB CHECK, to make it easy
// to add channels later).
const (
	KindDingTalk = "dingtalk" // DingTalk custom bot
	KindFeishu   = "feishu"   // Feishu (including Lark) custom bot
	KindWeCom    = "wecom"    // WeCom group bot
	KindWebhook  = "webhook"  // Generic Webhook: custom method/header/JSON template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP email
)

// Event types, corresponding to notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the fallback value for an empty kind in config.
const InitKind = KindDingTalk

// severityRank maps a finding's severity to a comparable ordinal.
// Unknown severities return 0, so any min_severity setting will
// block unknown severities — when in doubt, do not push, to avoid
// false-positive spam.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank returns the severity's ordinal; unknown severities
// return 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns the severity name with an emoji, used for
// the message title and card coloring. Unknown severities are echoed
// back as-is, no fabrication.
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

// StatusLabel translates a handling status into English, used in
// status-change messages.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "Todo"
	case "in_progress":
		return "In progress"
	case "confirmed":
		return "Confirmed"
	case "resolved":
		return "Resolved"
	case "fixed":
		return "Fixed"
	case "false_positive":
		return "False positive"
	case "ignored":
		return "Ignored"
	case "duplicate":
		return "Duplicate"
	case "risk_accepted":
		return "Risk accepted"
	default:
		return status
	}
}

// AtLeast determines whether severity meets the min threshold. When
// min is empty, no threshold is set and it always passes. Note that
// the ordinal of an unknown severity is 0, and will be rejected by
// any non-empty min (see severityRank's comment).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}

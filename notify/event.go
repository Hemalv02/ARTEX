package notify

// Snapshot is the contract for the notification_events.snapshot JSONB column.
// The writer is the finding-persistence transaction in the db layer; the
// readers are the delivery engine and filter matching in the server layer. The
// definition lives in this package because it is a "notification domain"
// payload: db only serializes it and does not understand the fields' meaning.
//
// Why finding fields are stored redundantly rather than looked up again at
// render time: a finding may later be renamed, re-graded, or have its status
// changed, while the pushed content should reflect the conclusion **as of the
// moment it happened** — a fresh lookup could dangerously mislead by returning
// "later downgraded to low". A side benefit is that fan-out and rendering need
// not JOIN the findings/tasks/assets tables.
type Snapshot struct {
	// Event type: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// Non-empty only when kind=finding_status_changed.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item is a single finding awaiting push, for a channel to render.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets are the resolved asset display names (e.g. domain/IP), filled in
	// by the server layer — this package never touches the database and cannot
	// obtain the names.
	Assets []string
	// DetailURL is the callback URL to the finding detail; empty means
	// public_base_url is not configured and it is omitted at render time.
	DetailURL string
	// Status-change events only; when both are non-empty it renders as
	// "Pending → Fixed".
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether the item is a status-change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the item's display title: prefer the manually assigned name,
// fall back to the vulnclass, and use a placeholder when both are empty — it
// never emits an empty title.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(unnamed finding)"
}

// Message is the complete content of one channel send.
type Message struct {
	// Length 1 for a single push; a whole batch for a digest push.
	// An empty slice is invalid; the caller must guarantee at least one item.
	Items []Item
	// When Batch=true, render as a digest message (different title, with the
	// time window and item count).
	Batch bool
	// WindowMinutes is the digest interval (in minutes), used only when
	// Batch=true for the "in the last N minutes" wording. It is deliberately
	// passed in explicitly from config rather than computed via time.Since at
	// render time, so rendering stays deterministic and testable.
	WindowMinutes int
	// HomeURL is the platform dashboard address (the global public_base_url);
	// empty means no dashboard entry link is included.
	HomeURL string
}

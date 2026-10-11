package notify

// Snapshot is the contract for the JSONB column
// notification_events.snapshot. The writer is the finding-persistence
// transaction in the db layer; the readers are the server-side
// delivery engine and the filter match. The definition lives in this
// package because it is a "notification-domain" payload: the db only
// handles serialization and does not understand the field meaning.
//
// Why the finding fields are stored redundantly rather than re-queried
// at render time: a finding is later renamed, re-leveled, and has its
// status changed, but the push content should reflect the conclusion
// **at the time of the event** — re-querying would yield the
// misleading "later changed to low". In addition, fan-out and
// rendering therefore do not have to JOIN three tables
// (findings/tasks/assets).
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

// Item is a single finding waiting to be pushed, for channel rendering.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets is the resolved display name of the asset (e.g. domain
	// /IP). Filled in by the server layer — this package does not
	// touch the database, so it cannot get the names.
	Assets []string
	// DetailURL is the link back to the finding detail; empty means
	// public_base_url is not configured, and the renderer omits it.
	DetailURL string
	// Reserved for status-change events; when both are non-empty it
	// is rendered as "Todo → Fixed".
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether the item is a status-change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the display title for the item: the manually named
// name takes priority, otherwise it falls back to the vulnerability
// type vulnclass; when both are empty a placeholder is used — never
// output an empty title.
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
	// Length is 1 for a single push; for a digest push it is the
	// whole batch. An empty slice is invalid; the caller must
	// guarantee at least one item.
	Items []Item
	// When Batch=true, render as a summary message (different title,
	// with time window and count).
	Batch bool
	// WindowMinutes is the summary cycle (minutes); used only when
	// Batch=true, for wording "in the last N minutes". Deliberately
	// passed in via configuration rather than computed as
	// time.Since at render time: rendering stays deterministic, so
	// it is testable.
	WindowMinutes int
	// HomeURL is the platform dashboard URL (global public_base_url);
	// empty means no dashboard entry is included.
	HomeURL string
}

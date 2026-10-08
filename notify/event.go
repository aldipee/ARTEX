package notify

// Snapshot is the contract for the notification_events.snapshot JSONB column. The
// writer is the vulnerability-storage transaction in the db layer; the readers are the
// delivery engine and filter matching in the server layer. It is defined in this
// package because it is a payload of the "notification domain": db only serializes it
// and does not understand field meanings.
//
// Why store the vulnerability fields redundantly instead of looking them up at render
// time: a vulnerability can later be renamed, re-leveled, or re-statused, but the push
// content should reflect the conclusion **at the time of the event** — looking it up
// would yield the dangerously misleading "was later changed to low". This also means
// fan-out and rendering don't need to JOIN the findings/tasks/assets tables.
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

// Item is a vulnerability pending push, for a channel to render.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets are the resolved asset display names (e.g. domain/IP). Filled by the
	// server layer — this package does not touch the database and cannot get names.
	Assets []string
	// DetailURL is the link back to the vulnerability details; empty means
	// public_base_url is not configured, and it is omitted at render time.
	DetailURL string
	// For status-change events only; when both are non-empty it renders as
	// "Pending → Fixed".
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether this item is a status-change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the item's display title: preferring the human-assigned name, falling
// back to the vulnerability class vulnclass, and using a placeholder when both are
// empty — never an empty title.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(Unnamed vulnerability)"
}

// Message is the complete content of a single channel send.
type Message struct {
	// Length 1 for a single push; a whole batch for a digest push. An empty slice is
	// illegal; the caller must guarantee at least one item.
	Items []Item
	// Batch=true renders as a digest message (different title, includes the time
	// window and count).
	Batch bool
	// WindowMinutes is the digest period (in minutes); used only when Batch=true for
	// the "last N minutes" wording. Deliberately passed in from config rather than
	// computed via time.Since at render time: rendering stays deterministic, which
	// makes it testable.
	WindowMinutes int
	// HomeURL is the platform dashboard address (the global public_base_url); empty
	// means no dashboard entry.
	HomeURL string
}

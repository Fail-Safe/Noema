package event

import "encoding/json"

// Action represents the kind of mutation applied to a Trace.
type Action string

const (
	ActionCreate    Action = "create"
	ActionUpdate    Action = "update"
	ActionArchive   Action = "archive"
	ActionUnarchive Action = "unarchive"
	ActionTrash     Action = "trash"
	ActionRecover   Action = "recover"
	ActionPurge     Action = "purge"
)

// Event is an immutable record of a mutation to a Trace.
//
// CortexID is the stable ULID identity of the cortex that produced the event.
// Origin is the human-readable display name at the time of writing — it can
// drift if the cortex is renamed and is never trusted for identity decisions
// on replay. Federation, vector clocks, and divergence detection all key on
// CortexID; Origin is purely for audit-trail rendering.
type Event struct {
	ID        string            `json:"id"`                  // ULID
	Action    Action            `json:"action"`
	TraceID   string            `json:"trace_id"`
	CortexID  string            `json:"cortex_id"`           // stable ULID identity (federation key)
	Origin    string            `json:"origin"`              // display name at write time
	Timestamp string            `json:"timestamp"`           // RFC3339 UTC
	Data      json.RawMessage   `json:"data,omitempty"`      // action-specific payload
	VClock    map[string]uint64 `json:"vclock,omitempty"`    // vector clock keyed on cortex IDs
}

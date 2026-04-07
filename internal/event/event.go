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
type Event struct {
	ID        string            `json:"id"`                // ULID
	Action    Action            `json:"action"`
	TraceID   string            `json:"trace_id"`
	Origin    string            `json:"origin"`            // cortex name that produced this event
	Timestamp string            `json:"timestamp"`         // RFC3339 UTC
	Data      json.RawMessage   `json:"data,omitempty"`    // action-specific payload
	VClock    map[string]uint64 `json:"vclock,omitempty"`  // vector clock (Phase 2)
}

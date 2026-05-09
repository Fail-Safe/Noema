package federation

// TraceUsage mirrors one row of the trace_usage table as exchanged
// over the sync_read_signal MCP tool. It lives in the federation
// package (not cortex) because the federation-side replayer interface
// needs to name this type, and federation → cortex would be a
// dependency cycle (cortex already imports federation).
//
// Read, modify, and search-hit counters are CRDT G-counters (grow-only,
// merged via MAX). LastReadAt is CRDT LWW (merged via MAX over RFC3339
// strings, which sort correctly). UpdatedAt is the sync cursor — each
// peer stamps its own local writes with an RFC3339 timestamp and callers
// pull rows where UpdatedAt > since.
//
// SearchHitCount uses `omitempty` so peers running a pre-migration-015
// binary stay wire-compatible: the field is dropped from outbound JSON
// when zero and decodes to zero from inbound payloads that omit it.
// MergeRemoteUsage applies the same MAX semantics it uses for
// read_count, so a missing field never overwrites a non-zero local
// value.
type TraceUsage struct {
	TraceID        string `json:"trace_id"`
	PeerCortexID   string `json:"peer_cortex_id"`
	ReadCount      int    `json:"read_count"`
	ModifyCount    int    `json:"modify_count"`
	SearchHitCount int    `json:"search_hit_count,omitempty"`
	LastReadAt     string `json:"last_read_at,omitempty"`
	UpdatedAt      string `json:"updated_at"`
}

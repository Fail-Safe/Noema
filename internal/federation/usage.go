package federation

// TraceUsage mirrors one row of the trace_usage table as exchanged
// over the sync_read_signal MCP tool. It lives in the federation
// package (not cortex) because the federation-side replayer interface
// needs to name this type, and federation → cortex would be a
// dependency cycle (cortex already imports federation).
//
// Read and modify counters are CRDT G-counters (grow-only, merged via
// MAX). LastReadAt is CRDT LWW (merged via MAX over RFC3339 strings,
// which sort correctly). UpdatedAt is the sync cursor — each peer
// stamps its own local writes with an RFC3339 timestamp and callers
// pull rows where UpdatedAt > since.
type TraceUsage struct {
	TraceID      string `json:"trace_id"`
	PeerCortexID string `json:"peer_cortex_id"`
	ReadCount    int    `json:"read_count"`
	ModifyCount  int    `json:"modify_count"`
	LastReadAt   string `json:"last_read_at,omitempty"`
	UpdatedAt    string `json:"updated_at"`
}

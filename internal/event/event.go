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

	// Memory-tiering actions. See docs/plans/consolidation-plan.md.
	// These constants ship in Phase 2 so federation event replay doesn't
	// encounter unknown actions mid-rollout once later phases start
	// emitting them.
	ActionPromote             Action = "promote"
	ActionDemote              Action = "demote"
	ActionConsolidate         Action = "consolidate"
	ActionConsolidateFallback Action = "consolidate_fallback"
	ActionDivergenceLongTerm  Action = "divergence_long_term"
	ActionVote                Action = "vote"
	ActionPurgeLongTerm       Action = "purge_long_term"
	ActionPurgeHard           Action = "purge_hard"

	// Multi-peer consolidation coordination. See consolidation-plan.md §14.
	// Ship alongside the rank-advertisement foundation so federation event
	// replay doesn't encounter unknown actions once later phases start
	// emitting them.
	ActionConsolidationClaim   Action = "consolidation_claim"
	ActionConsolidationSuccess Action = "consolidation_success"
	ActionConsolidationFail    Action = "consolidation_fail"
)

// Event is an immutable record of a mutation to a Trace.
//
// CortexID is the stable ULID identity of the cortex that produced the event.
// Origin is the human-readable display name at the time of writing — it can
// drift if the cortex is renamed and is never trusted for identity decisions
// on replay. Federation, vector clocks, and divergence detection all key on
// CortexID; Origin is purely for audit-trail rendering.
type Event struct {
	ID        string            `json:"id"` // ULID
	Action    Action            `json:"action"`
	TraceID   string            `json:"trace_id"`
	CortexID  string            `json:"cortex_id"`        // stable ULID identity (federation key)
	Origin    string            `json:"origin"`           // display name at write time
	Timestamp string            `json:"timestamp"`        // RFC3339 UTC
	Data      json.RawMessage   `json:"data,omitempty"`   // action-specific payload
	VClock    map[string]uint64 `json:"vclock,omitempty"` // vector clock keyed on cortex IDs

	// Signature authenticates the originating cortex. It is the
	// "ed25519:<base64>" form defined by the federation signing wire spec,
	// computed over the canonical preimage of every other field. Empty
	// for events produced before signing was configured; how empty or
	// invalid signatures are treated is governed by the federation verify
	// mode (off | warn | enforce). It is never part of its own preimage.
	Signature string `json:"signature,omitempty"`

	// PubKey is the signer's "ed25519:<base64>" public key, carried so that
	// a verifier can check an event whose CortexID it has never directly
	// handshaked — federation gossips events transitively, so the signing
	// key has to travel with the event. Like Signature, it is identifying
	// metadata, NOT part of the signed preimage: an event claiming a
	// CortexID that is already pinned must verify under the *pinned* key, so
	// a forged PubKey can't impersonate a known cortex. Empty for unsigned
	// events.
	PubKey string `json:"pubkey,omitempty"`
}

// NormalizeData returns the canonical on-the-wire form of an event payload.
// A nil or empty payload becomes the two bytes `{}`. Both the event store
// (before persistence) and the signing preimage builder MUST use this so the
// bytes that get stored, transmitted, and signed are byte-identical.
func NormalizeData(data json.RawMessage) json.RawMessage {
	if len(data) == 0 {
		return json.RawMessage("{}")
	}
	return data
}

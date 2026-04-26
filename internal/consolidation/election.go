package consolidation

import (
	"fmt"
	"time"

	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/federation"
)

// EventEmitter is the narrow subset of cortex.Cortex that Election needs
// for emitting coordination events. Defining it here lets tests mock the
// emission surface without standing up a real Cortex.
type EventEmitter interface {
	EmitCoordinationEvent(action event.Action, windowID string, data any) error
}

// ElectionConfig is the runtime input for an Election evaluator.
type ElectionConfig struct {
	// CortexID is this cortex's stable ULID. Decide compares it against
	// the winning CortexID to classify self-won vs. other-won vs. none.
	CortexID string

	// PeerNames lists federated peers whose ranks should be gathered
	// from federation_state when Decide is called. Pass an empty slice
	// for single-node cortexes; Decide degenerates to "we're the only
	// eligible peer" automatically.
	PeerNames []string

	// QuietPeriod is the minimum age (now - ObservedAt) for a rank
	// entry to count toward the election. Matches 2 × federation.
	// interval per the plan. Callers in tests may pass zero to disable.
	QuietPeriod time.Duration

	// Now is injected for tests; zero defaults to time.Now.
	Now func() time.Time

	// State provides read access to rank entries. Required.
	State *federation.State

	// Emitter is the target for Claim / Success / Fail events.
	// Required.
	Emitter EventEmitter

	// Log is the optional logger; nil is a safe no-op.
	Log func(format string, args ...any)
}

// Outcome is the result of one Decide call.
type Outcome struct {
	// ShouldRun is true when the caller should proceed with the pass.
	ShouldRun bool

	// Winner is the CortexID of the elected peer, or empty if no one
	// qualified.
	Winner string

	// Reason is a short human-readable explanation for logs.
	Reason string
}

// Election evaluates rank advertisements into a skip/run decision and
// emits the coordination events that drive federation-wide consensus.
// One Election per Agent; safe for concurrent use because all state
// lives in federation_state rather than in the struct.
type Election struct {
	cfg ElectionConfig
}

// NewElection constructs an evaluator with defaults filled in.
func NewElection(cfg ElectionConfig) *Election {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &Election{cfg: cfg}
}

// CortexID returns the configured local cortex ID. Exposed so callers
// holding only an *Election can label log lines without plumbing the
// ID separately.
func (e *Election) CortexID() string {
	return e.cfg.CortexID
}

// QuietPeriod returns the configured quiet-period duration. Exposed
// for the pass-gate wrapper to sleep between Claim and the recheck.
func (e *Election) QuietPeriod() time.Duration {
	return e.cfg.QuietPeriod
}

// Decide evaluates the current rank view and returns whether this peer
// should proceed with a consolidation pass. No side effects — Claim /
// Success / Fail are emitted by the caller after observing the
// outcome, so tests can exercise the decision logic in isolation.
func (e *Election) Decide() Outcome {
	now := e.cfg.Now()
	entries := e.gatherEntries()
	winner := ElectWinner(entries, e.cfg.QuietPeriod, now)
	if winner == "" {
		return Outcome{ShouldRun: false, Reason: "no eligible peer"}
	}
	if winner != e.cfg.CortexID {
		return Outcome{
			ShouldRun: false,
			Winner:    winner,
			Reason:    fmt.Sprintf("peer %s won", winner),
		}
	}
	return Outcome{ShouldRun: true, Winner: winner, Reason: "winner"}
}

// gatherEntries collects local + all configured peer RankEntries from
// federation_state. Load errors per entry are swallowed — a peer we
// can't read is equivalent to a peer we haven't heard from, which
// ElectWinner already handles as ineligible.
func (e *Election) gatherEntries() []federation.RankEntry {
	entries := make([]federation.RankEntry, 0, len(e.cfg.PeerNames)+1)
	if local, err := e.cfg.State.GetLocalRank(); err == nil {
		entries = append(entries, local)
	}
	for _, name := range e.cfg.PeerNames {
		if r, err := e.cfg.State.GetPeerRank(name); err == nil {
			entries = append(entries, r)
		}
	}
	return entries
}

// ClaimData is the payload for ActionConsolidationClaim.
type ClaimData struct {
	WindowID string `json:"window_id"`
	CortexID string `json:"cortex_id"`
}

// SuccessData is the payload for ActionConsolidationSuccess.
// Distillation and promotion counts are optional in v1; Phase 4 wires
// them in when the pass function's return signature is enriched.
type SuccessData struct {
	WindowID             string `json:"window_id"`
	CortexID             string `json:"cortex_id"`
	DistillationsCreated int    `json:"distillations_created,omitempty"`
	SourcesPromoted      int    `json:"sources_promoted,omitempty"`
}

// FailData is the payload for ActionConsolidationFail.
type FailData struct {
	WindowID string `json:"window_id"`
	CortexID string `json:"cortex_id"`
	Reason   string `json:"reason"`
}

// Fail-reason enum, matching the plan's normalized reason strings.
// Kept as string constants so callers can compose new reasons without
// a package change.
//
// The three "preempted" reasons (PeerOutranked, NoWinnerAtRecheck,
// ContextCanceled) replace the older catch-all "aborted_by_peer_conflict"
// reason that was emitted by all three quiet-period exit paths. Telling
// them apart in the event log lets operators distinguish "ai-3 outranked
// us during the wait" from "everyone's rank entry expired" from
// "agent.Stop() interrupted us" — same outcome (no pass), wildly
// different operational meaning.
const (
	FailReasonEndpointDown      = "endpoint_down"
	FailReasonLLMError          = "llm_error"
	FailReasonValidationFailed  = "validation_failed"
	FailReasonPeerOutranked     = "peer_outranked"
	FailReasonNoWinnerAtRecheck = "no_winner_at_recheck"
	FailReasonContextCanceled   = "context_canceled"
	FailReasonWatchdogExpired   = "watchdog_expired"
)

// Claim records this peer's intent to run the pass for windowID. Must
// be emitted before pass execution starts so observers can watchdog
// off the event timestamp in future phases.
func (e *Election) Claim(windowID string) error {
	return e.cfg.Emitter.EmitCoordinationEvent(
		event.ActionConsolidationClaim, windowID,
		ClaimData{WindowID: windowID, CortexID: e.cfg.CortexID},
	)
}

// Success records a completed pass. Emitted only by the winner after
// the inner pass function returns without error.
func (e *Election) Success(windowID string, distillations, sourcesPromoted int) error {
	return e.cfg.Emitter.EmitCoordinationEvent(
		event.ActionConsolidationSuccess, windowID,
		SuccessData{
			WindowID:             windowID,
			CortexID:             e.cfg.CortexID,
			DistillationsCreated: distillations,
			SourcesPromoted:      sourcesPromoted,
		},
	)
}

// Fail records an aborted or errored pass. Emitted by the winner when
// the inner pass returns an error, by the gate when a preemption is
// detected during the quiet-period recheck, and by future watchdog
// code when a winner fails to report within its expected duration.
func (e *Election) Fail(windowID, reason string) error {
	return e.cfg.Emitter.EmitCoordinationEvent(
		event.ActionConsolidationFail, windowID,
		FailData{WindowID: windowID, CortexID: e.cfg.CortexID, Reason: reason},
	)
}

package consolidation

import (
	"context"
	"sync"
	"time"

	"github.com/Fail-Safe/Noema/internal/federation"
)

// defaultCheckInterval is the cadence at which the eligibility loop re-
// probes its local LLM endpoint and refreshes the advertised rank. The
// value is an internal constant rather than user config — see
// consolidation-plan.md §14 "Configuration surface" for the rationale.
const defaultCheckInterval = 15 * time.Minute

// EligibilityConfig carries the runtime inputs for an EligibilityLoop.
// Callers build one in cmd_serve from the cortex manifest and the
// federation state handle.
type EligibilityConfig struct {
	// Enabled reflects cortex.md consolidation.enabled. When false the
	// loop advertises Rank=0 unconditionally so peers see this cortex as
	// opted out.
	Enabled bool

	// LLMEnabled reflects cortex.md consolidation.llm_enabled. When false
	// the loop also advertises Rank=0 — a cortex without an LLM cannot
	// run distillation passes and so cannot legitimately win a round.
	LLMEnabled bool

	// FederationMode is the effective federation mode of this cortex
	// (sync / publish / subscribe). When "subscribe", the loop forces
	// Rank=0 because a read-only mirror cannot legitimately run a
	// consolidation pass — matches the §14 mode table in the plan.
	// Empty string is treated as "sync" (the EffectiveMode default).
	FederationMode string

	// Endpoint is the OpenAI-compatible base URL probed on every tick.
	// Empty endpoint is treated as unreachable.
	Endpoint string

	// CortexID is this cortex's stable ULID, stamped into every
	// RankEntry so remote peers can attribute the advertisement.
	CortexID string

	// CheckInterval is the probe cadence. Zero defaults to
	// defaultCheckInterval (15 min).
	CheckInterval time.Duration

	// Now injects a clock for tests. Zero defaults to time.Now.
	Now func() time.Time

	// Probe injects a probe function for tests. Zero defaults to
	// ProbeEndpoint with a real HTTP client.
	Probe func(ctx context.Context, endpoint string) bool

	// State persists the advertised RankEntry in federation_state.
	// Required; constructors that receive a nil State will panic at
	// first write rather than silently no-op.
	State *federation.State

	// Log is the optional logger; nil is a safe no-op.
	Log func(format string, args ...any)
}

// EligibilityLoop refreshes this peer's consolidation rank on a cadence.
// One loop per cortex, lifecycle parallel to Agent. Writes the
// advertised RankEntry into federation_state every cycle.
//
// The loop re-rolls the Rank value on every refresh while the peer is
// eligible — matches consolidation-plan.md §14 step 5 (eligibility check
// unconditionally sets rank to a fresh random 1..99). Stability within
// an election is provided by the quiet-period filter in ElectWinner,
// not by preserving ranks across ticks; re-rolling every cycle gives
// leadership rotation across windows for free without a separate
// "reset on Success event" handler.
type EligibilityLoop struct {
	cfg EligibilityConfig

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewEligibilityLoop constructs a loop with defaults filled in. Call
// Start to begin refreshing; call Stop to drain.
func NewEligibilityLoop(cfg EligibilityConfig) *EligibilityLoop {
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = defaultCheckInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Probe == nil {
		cfg.Probe = ProbeEndpoint
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &EligibilityLoop{cfg: cfg}
}

// Start kicks off the background loop. Call exactly once per loop;
// subsequent Start calls after Stop require a fresh NewEligibilityLoop.
func (e *EligibilityLoop) Start() {
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.wg.Add(1)
	go e.loop()
}

// Stop signals the loop to exit and blocks until it does. Safe to call
// even if Start never ran.
func (e *EligibilityLoop) Stop() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	e.wg.Wait()
}

func (e *EligibilityLoop) loop() {
	defer e.wg.Done()

	// Fire a refresh immediately so rank is populated before the first
	// sync cycle can observe a missing value.
	e.refresh()

	ticker := time.NewTicker(e.cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			e.refresh()
		}
	}
}

// Refresh runs one cycle synchronously. Exported for tests and for
// callers that want to force a rank update after a config change
// without waiting for the next tick.
func (e *EligibilityLoop) Refresh() {
	e.refresh()
}

func (e *EligibilityLoop) refresh() {
	now := e.cfg.Now()
	newEntry := federation.RankEntry{
		CortexID:   e.cfg.CortexID,
		ObservedAt: now.UTC().Format(time.RFC3339),
	}

	switch {
	case !e.cfg.Enabled, !e.cfg.LLMEnabled:
		// Feature disabled by config. Advertise ineligibility so peers
		// know this cortex is deliberately not participating.
		newEntry.Rank = RankIneligible
	case e.cfg.FederationMode == "subscribe":
		// Read-only mirror mode (plan §14 table). A subscribe-mode
		// cortex pulls events from others but cannot serve them; by
		// extension it should not author distillations either. Force
		// Rank=0 regardless of probe outcome.
		newEntry.Rank = RankIneligible
	case !e.cfg.Probe(e.ctx, e.cfg.Endpoint):
		// Endpoint unreachable; demote to ineligible until it recovers.
		newEntry.Rank = RankIneligible
		e.cfg.Log("[consolidation] endpoint probe failed; rank=0")
	default:
		// Endpoint alive. Roll a fresh bid on every refresh — the plan
		// spec is unconditional re-roll, and election correctness is
		// provided by the quiet-period filter, not by rank stability.
		r, gerr := GenerateRank()
		if gerr != nil {
			e.cfg.Log("[consolidation] generate rank failed: %v", gerr)
			newEntry.Rank = RankIneligible
		} else {
			newEntry.Rank = r
		}
	}

	if err := e.cfg.State.SetLocalRank(newEntry); err != nil {
		e.cfg.Log("[consolidation] persist rank failed: %v", err)
	}
}

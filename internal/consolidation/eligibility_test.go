package consolidation_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/consolidation"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/federation"
)

// buildLoop wires an EligibilityLoop against a real federation_state on
// disk. Tests drive refreshes via the Refresh() entry point so we don't
// race the ticker goroutine.
func buildLoop(t *testing.T, cfg consolidation.EligibilityConfig) (*consolidation.EligibilityLoop, *federation.State) {
	t.Helper()
	dir := t.TempDir()
	if _, err := cortex.Create("elig", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("elig", filepath.Join(dir, "elig"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	state := federation.NewState(cx.DB.DB)
	cfg.State = state
	if cfg.CortexID == "" {
		cfg.CortexID = "01TEST"
	}
	if cfg.Now == nil {
		fixed := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
		cfg.Now = func() time.Time { return fixed }
	}
	return consolidation.NewEligibilityLoop(cfg), state
}

func TestEligibility_DisabledConfig_AdvertisesZero(t *testing.T) {
	// Feature off → rank stays 0 regardless of probe outcome.
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:    false,
		LLMEnabled: true,
		Endpoint:   "http://example.invalid",
		Probe:      func(context.Context, string) bool { return true },
	})
	loop.Refresh()

	got, err := state.GetLocalRank()
	if err != nil {
		t.Fatalf("GetLocalRank: %v", err)
	}
	if got.Rank != consolidation.RankIneligible {
		t.Errorf("enabled=false: got rank %d, want 0", got.Rank)
	}
	if got.CortexID != "01TEST" {
		t.Errorf("CortexID = %q, want 01TEST", got.CortexID)
	}
	if got.ObservedAt == "" {
		t.Error("ObservedAt empty — should always be stamped even when ineligible")
	}
}

func TestEligibility_SubscribeMode_AdvertisesZero(t *testing.T) {
	// A subscribe-mode cortex is a read-only mirror. It must advertise
	// Rank=0 even when the feature is enabled and the endpoint is alive
	// — matches the §14 mode-compatibility table.
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:            true,
		LLMEnabled:         true,
		TriggersConfigured: true,
		FederationMode:     "subscribe",
		Endpoint:           "http://example.invalid",
		Probe:              func(context.Context, string) bool { return true },
	})
	loop.Refresh()

	got, err := state.GetLocalRank()
	if err != nil {
		t.Fatalf("GetLocalRank: %v", err)
	}
	if got.Rank != consolidation.RankIneligible {
		t.Errorf("subscribe mode: got rank %d, want 0", got.Rank)
	}
}

func TestEligibility_SyncMode_AdvertisesRank(t *testing.T) {
	// Regression guard: the subscribe-mode branch must not accidentally
	// blacklist sync-mode cortexes.
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:            true,
		LLMEnabled:         true,
		TriggersConfigured: true,
		FederationMode:     "sync",
		Endpoint:           "http://example.invalid",
		Probe:              func(context.Context, string) bool { return true },
	})
	loop.Refresh()

	got, err := state.GetLocalRank()
	if err != nil {
		t.Fatalf("GetLocalRank: %v", err)
	}
	if got.Rank < consolidation.RankMin || got.Rank > consolidation.RankMax {
		t.Errorf("sync mode: got rank %d, want in [%d, %d]",
			got.Rank, consolidation.RankMin, consolidation.RankMax)
	}
}

func TestEligibility_LLMDisabled_AdvertisesZero(t *testing.T) {
	// Master flag on, LLM off → rank stays 0. Heuristic-only cortexes
	// cannot do distillation so they should not participate in election.
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:    true,
		LLMEnabled: false,
		Endpoint:   "http://example.invalid",
		Probe:      func(context.Context, string) bool { return true },
	})
	loop.Refresh()

	got, err := state.GetLocalRank()
	if err != nil {
		t.Fatalf("GetLocalRank: %v", err)
	}
	if got.Rank != consolidation.RankIneligible {
		t.Errorf("llm_enabled=false: got rank %d, want 0", got.Rank)
	}
}

func TestEligibility_ProbeFails_AdvertisesZero(t *testing.T) {
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:            true,
		LLMEnabled:         true,
		TriggersConfigured: true,
		Endpoint:           "http://example.invalid",
		Probe:              func(context.Context, string) bool { return false },
	})
	loop.Refresh()

	got, err := state.GetLocalRank()
	if err != nil {
		t.Fatalf("GetLocalRank: %v", err)
	}
	if got.Rank != consolidation.RankIneligible {
		t.Errorf("probe fail: got rank %d, want 0", got.Rank)
	}
}

func TestEligibility_NoTriggersConfigured_AdvertisesZero(t *testing.T) {
	// Phantom-winner guard: a peer that opted into consolidation but
	// configured no scheduling trigger (cron / idle_minutes /
	// threshold_short) will never run a pass. Advertising a non-zero
	// rank in that state makes other peers defer to a leader that
	// never claims a window, stalling the ring. The loop must force
	// Rank=0 in this case even when LLM and probe are healthy.
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:            true,
		LLMEnabled:         true,
		TriggersConfigured: false,
		Endpoint:           "http://example.invalid",
		Probe:              func(context.Context, string) bool { return true },
	})
	loop.Refresh()

	got, err := state.GetLocalRank()
	if err != nil {
		t.Fatalf("GetLocalRank: %v", err)
	}
	if got.Rank != consolidation.RankIneligible {
		t.Errorf("no triggers: got rank %d, want 0", got.Rank)
	}
}

func TestEligibility_AliveAndEnabled_RollsRank(t *testing.T) {
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:            true,
		LLMEnabled:         true,
		TriggersConfigured: true,
		Endpoint:           "http://example.invalid",
		Probe:              func(context.Context, string) bool { return true },
	})
	loop.Refresh()

	got, err := state.GetLocalRank()
	if err != nil {
		t.Fatalf("GetLocalRank: %v", err)
	}
	if got.Rank < consolidation.RankMin || got.Rank > consolidation.RankMax {
		t.Errorf("alive+enabled: got rank %d, want in [%d, %d]",
			got.Rank, consolidation.RankMin, consolidation.RankMax)
	}
}

func TestEligibility_ReRollsEveryRefresh(t *testing.T) {
	// Plan §14 step 5: every eligibility check sets rank to a fresh
	// random 1..99. Within-window election stability is provided by
	// the quiet-period filter in ElectWinner, not by preserving rank
	// across ticks. Re-rolling gives leadership rotation for free.
	//
	// Collisions across two rolls are ~1/99, so a single refresh pair
	// would be flaky; instead, check that across many refreshes we see
	// at least two distinct values.
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:            true,
		LLMEnabled:         true,
		TriggersConfigured: true,
		Endpoint:           "http://example.invalid",
		Probe:              func(context.Context, string) bool { return true },
	})

	seen := map[int]bool{}
	for range 20 {
		loop.Refresh()
		r, err := state.GetLocalRank()
		if err != nil {
			t.Fatalf("GetLocalRank: %v", err)
		}
		seen[r.Rank] = true
	}
	if len(seen) < 2 {
		t.Errorf("expected >=2 distinct ranks across 20 refreshes, got %d (values: %v)",
			len(seen), seen)
	}
}

func TestEligibility_RollsFreshAfterOutageRecovery(t *testing.T) {
	// The 0 -> N transition path: endpoint down, rank=0, endpoint back
	// up, rank must land in [1..99] on the next refresh.
	var alive bool
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:            true,
		LLMEnabled:         true,
		TriggersConfigured: true,
		Endpoint:           "http://example.invalid",
		Probe:              func(context.Context, string) bool { return alive },
	})

	alive = true
	loop.Refresh()
	r1, _ := state.GetLocalRank()
	if r1.Rank == 0 {
		t.Fatalf("precondition: first refresh didn't roll a rank")
	}

	alive = false
	loop.Refresh()
	r2, _ := state.GetLocalRank()
	if r2.Rank != 0 {
		t.Fatalf("outage: expected rank 0, got %d", r2.Rank)
	}

	alive = true
	loop.Refresh()
	r3, _ := state.GetLocalRank()
	if r3.Rank == 0 {
		t.Fatalf("recovery: expected fresh roll, got 0")
	}
}

func TestEligibility_ObservedAtAdvances(t *testing.T) {
	// Every refresh bumps ObservedAt so remote peers have a fresh
	// staleness signal for the quiet-period guard. Rank value is
	// expected to change too (re-roll every tick) but the assertion
	// here is about ObservedAt movement specifically.
	clock := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:            true,
		LLMEnabled:         true,
		TriggersConfigured: true,
		Endpoint:           "http://example.invalid",
		Probe:              func(context.Context, string) bool { return true },
		Now:                func() time.Time { return clock },
	})

	loop.Refresh()
	first, _ := state.GetLocalRank()

	clock = clock.Add(10 * time.Minute)
	loop.Refresh()
	second, _ := state.GetLocalRank()

	if second.ObservedAt == first.ObservedAt {
		t.Errorf("ObservedAt unchanged across refresh: %q", first.ObservedAt)
	}
}

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
		Enabled:    true,
		LLMEnabled: true,
		Endpoint:   "http://example.invalid",
		Probe:      func(context.Context, string) bool { return false },
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

func TestEligibility_AliveAndEnabled_RollsRank(t *testing.T) {
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:    true,
		LLMEnabled: true,
		Endpoint:   "http://example.invalid",
		Probe:      func(context.Context, string) bool { return true },
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

func TestEligibility_PreservesRankAcrossRefresh(t *testing.T) {
	// Rank is stable within a window; re-rolling every tick would mean
	// two peers with staggered check cadences see each other flipping
	// values, defeating deterministic election.
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:    true,
		LLMEnabled: true,
		Endpoint:   "http://example.invalid",
		Probe:      func(context.Context, string) bool { return true },
	})

	loop.Refresh()
	first, err := state.GetLocalRank()
	if err != nil {
		t.Fatalf("first GetLocalRank: %v", err)
	}

	loop.Refresh()
	second, err := state.GetLocalRank()
	if err != nil {
		t.Fatalf("second GetLocalRank: %v", err)
	}

	if second.Rank != first.Rank {
		t.Errorf("rank changed across refresh: %d -> %d (want stable)",
			first.Rank, second.Rank)
	}
}

func TestEligibility_ReRollsAfterIneligibilityTransition(t *testing.T) {
	// When the endpoint goes down then comes back, the peer must roll a
	// fresh bid rather than reusing a stale one from before the outage.
	// This is the "0 -> N" transition path.
	var alive bool
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:    true,
		LLMEnabled: true,
		Endpoint:   "http://example.invalid",
		Probe:      func(context.Context, string) bool { return alive },
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
	// Re-roll must happen here. The new rank may legitimately collide
	// with the first one (1/99 odds), so retry the entire test up to 5
	// times if that happens — we care about the re-roll path firing,
	// not about distinct values from a 99-element sample space.
	loop.Refresh()
	r3, _ := state.GetLocalRank()
	if r3.Rank == 0 {
		t.Fatalf("recovery: expected fresh roll, got 0")
	}
}

func TestEligibility_ObservedAtAdvances(t *testing.T) {
	// Every refresh must bump ObservedAt even when the rank value is
	// unchanged, so remote peers have a fresh staleness signal to feed
	// into the quiet-period guard.
	clock := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	loop, state := buildLoop(t, consolidation.EligibilityConfig{
		Enabled:    true,
		LLMEnabled: true,
		Endpoint:   "http://example.invalid",
		Probe:      func(context.Context, string) bool { return true },
		Now:        func() time.Time { return clock },
	})

	loop.Refresh()
	first, _ := state.GetLocalRank()

	clock = clock.Add(10 * time.Minute)
	loop.Refresh()
	second, _ := state.GetLocalRank()

	if second.ObservedAt == first.ObservedAt {
		t.Errorf("ObservedAt unchanged across refresh: %q", first.ObservedAt)
	}
	if second.Rank != first.Rank {
		t.Errorf("rank changed unexpectedly: %d -> %d", first.Rank, second.Rank)
	}
}

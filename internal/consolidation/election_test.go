package consolidation_test

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/consolidation"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/federation"
)

// recordedEvent captures one emitted coordination event for assertions.
type recordedEvent struct {
	Action   event.Action
	WindowID string
	Data     any
}

// fakeEmitter implements consolidation.EventEmitter and remembers every
// call. Thread-safe so the pass-gate tests can use it in concurrent
// flows later.
type fakeEmitter struct {
	mu     sync.Mutex
	events []recordedEvent
	err    error // optional return for EmitCoordinationEvent
}

func (f *fakeEmitter) EmitCoordinationEvent(action event.Action, windowID string, data any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{Action: action, WindowID: windowID, Data: data})
	return f.err
}

func (f *fakeEmitter) count(action event.Action) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e.Action == action {
			n++
		}
	}
	return n
}

// newElectionState provides a federation.State backed by a real cortex
// so rank reads hit the actual kv table.
func newElectionState(t *testing.T) *federation.State {
	t.Helper()
	dir := t.TempDir()
	if _, err := cortex.Create("election", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("election", filepath.Join(dir, "election"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return federation.NewState(cx.DB.DB)
}

func stale(t time.Time) string {
	return t.Add(-time.Hour).UTC().Format(time.RFC3339)
}

func TestDecide_NoEligiblePeers(t *testing.T) {
	// Everyone advertises Rank=0 → election returns skip with "no
	// eligible peer" reason.
	state := newElectionState(t)
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	must(t, state.SetLocalRank(federation.RankEntry{CortexID: "01LOCAL", Rank: 0, ObservedAt: stale(now)}))
	must(t, state.SetPeerRank("ai-2", federation.RankEntry{CortexID: "01PEER", Rank: 0, ObservedAt: stale(now)}))

	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID:  "01LOCAL",
		PeerNames: []string{"ai-2"},
		Now:       func() time.Time { return now },
		State:     state,
		Emitter:   &fakeEmitter{},
	})

	o := e.Decide()
	if o.ShouldRun {
		t.Errorf("all ineligible: ShouldRun=true, want false")
	}
	if o.Winner != "" {
		t.Errorf("winner = %q, want empty", o.Winner)
	}
}

func TestDecide_SingleNodeWinsTrivially(t *testing.T) {
	// A cortex with no peers configured and its own rank > 0 always
	// wins. This is the single-node degenerate case.
	state := newElectionState(t)
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	must(t, state.SetLocalRank(federation.RankEntry{CortexID: "01LOCAL", Rank: 42, ObservedAt: stale(now)}))

	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID:  "01LOCAL",
		PeerNames: nil,
		Now:       func() time.Time { return now },
		State:     state,
		Emitter:   &fakeEmitter{},
	})

	o := e.Decide()
	if !o.ShouldRun {
		t.Errorf("single-node: ShouldRun=false, want true (reason: %q)", o.Reason)
	}
	if o.Winner != "01LOCAL" {
		t.Errorf("winner = %q, want 01LOCAL", o.Winner)
	}
}

func TestDecide_HigherRankedPeerWins(t *testing.T) {
	state := newElectionState(t)
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	must(t, state.SetLocalRank(federation.RankEntry{CortexID: "01LOCAL", Rank: 30, ObservedAt: stale(now)}))
	must(t, state.SetPeerRank("ai-2", federation.RankEntry{CortexID: "01PEER", Rank: 80, ObservedAt: stale(now)}))

	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID:  "01LOCAL",
		PeerNames: []string{"ai-2"},
		Now:       func() time.Time { return now },
		State:     state,
		Emitter:   &fakeEmitter{},
	})

	o := e.Decide()
	if o.ShouldRun {
		t.Errorf("local outranked: ShouldRun=true, want false")
	}
	if o.Winner != "01PEER" {
		t.Errorf("winner = %q, want 01PEER", o.Winner)
	}
}

func TestDecide_LocalWinsOnTiebreak(t *testing.T) {
	// Same rank, local cortex_id > peer cortex_id → local wins.
	state := newElectionState(t)
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	must(t, state.SetLocalRank(federation.RankEntry{CortexID: "01ZZZ", Rank: 50, ObservedAt: stale(now)}))
	must(t, state.SetPeerRank("ai-2", federation.RankEntry{CortexID: "01AAA", Rank: 50, ObservedAt: stale(now)}))

	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID:  "01ZZZ",
		PeerNames: []string{"ai-2"},
		Now:       func() time.Time { return now },
		State:     state,
		Emitter:   &fakeEmitter{},
	})

	o := e.Decide()
	if !o.ShouldRun {
		t.Errorf("local tiebreak win: ShouldRun=false, want true (reason: %q)", o.Reason)
	}
	if o.Winner != "01ZZZ" {
		t.Errorf("winner = %q, want 01ZZZ", o.Winner)
	}
}

func TestDecide_QuietPeriodExcludesFreshEntries(t *testing.T) {
	// A fresh high-rank entry is excluded by the quiet-period filter; a
	// stale lower-rank entry wins.
	state := newElectionState(t)
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	must(t, state.SetLocalRank(federation.RankEntry{
		CortexID:   "01LOCAL",
		Rank:       99,
		ObservedAt: now.Add(-5 * time.Second).UTC().Format(time.RFC3339), // fresh
	}))
	must(t, state.SetPeerRank("ai-2", federation.RankEntry{
		CortexID:   "01PEER",
		Rank:       30,
		ObservedAt: now.Add(-time.Hour).UTC().Format(time.RFC3339), // stale
	}))

	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID:    "01LOCAL",
		PeerNames:   []string{"ai-2"},
		QuietPeriod: time.Minute,
		Now:         func() time.Time { return now },
		State:       state,
		Emitter:     &fakeEmitter{},
	})

	o := e.Decide()
	if o.ShouldRun {
		t.Errorf("local fresh rank: ShouldRun=true, want false (quiet period should exclude it)")
	}
	if o.Winner != "01PEER" {
		t.Errorf("winner = %q, want 01PEER (stale but valid)", o.Winner)
	}
}

func TestClaim_EmitsEvent(t *testing.T) {
	state := newElectionState(t)
	emitter := &fakeEmitter{}
	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID: "01LOCAL",
		State:    state,
		Emitter:  emitter,
	})

	if err := e.Claim("01WINDOW"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if emitter.count(event.ActionConsolidationClaim) != 1 {
		t.Errorf("claim events = %d, want 1", emitter.count(event.ActionConsolidationClaim))
	}
}

func TestSuccess_EmitsEventWithStats(t *testing.T) {
	state := newElectionState(t)
	emitter := &fakeEmitter{}
	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID: "01LOCAL",
		State:    state,
		Emitter:  emitter,
	})

	if err := e.Success("01WINDOW", 3, 7); err != nil {
		t.Fatalf("Success: %v", err)
	}
	if emitter.count(event.ActionConsolidationSuccess) != 1 {
		t.Errorf("success events = %d, want 1", emitter.count(event.ActionConsolidationSuccess))
	}

	data, ok := emitter.events[0].Data.(consolidation.SuccessData)
	if !ok {
		t.Fatalf("data type = %T, want SuccessData", emitter.events[0].Data)
	}
	if data.DistillationsCreated != 3 || data.SourcesPromoted != 7 {
		t.Errorf("stats = (%d, %d), want (3, 7)",
			data.DistillationsCreated, data.SourcesPromoted)
	}
}

func TestFail_PropagatesReason(t *testing.T) {
	state := newElectionState(t)
	emitter := &fakeEmitter{}
	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID: "01LOCAL",
		State:    state,
		Emitter:  emitter,
	})

	if err := e.Fail("01WINDOW", consolidation.FailReasonEndpointDown); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	data, ok := emitter.events[0].Data.(consolidation.FailData)
	if !ok {
		t.Fatalf("data type = %T, want FailData", emitter.events[0].Data)
	}
	if data.Reason != consolidation.FailReasonEndpointDown {
		t.Errorf("reason = %q, want %q", data.Reason, consolidation.FailReasonEndpointDown)
	}
}

func TestClaim_PropagatesEmitterError(t *testing.T) {
	state := newElectionState(t)
	emitter := &fakeEmitter{err: errors.New("sql unavailable")}
	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID: "01LOCAL",
		State:    state,
		Emitter:  emitter,
	})

	if err := e.Claim("01WINDOW"); err == nil {
		t.Error("expected emitter error to propagate, got nil")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

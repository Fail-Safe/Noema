package consolidation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/consolidation"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/federation"
)

// callTracker records each invocation of the wrapped pass function.
type callTracker struct {
	calls int
	err   error
}

func (c *callTracker) pass(context.Context, string) error {
	c.calls++
	return c.err
}

func TestWithElection_SkipsWhenNotElected(t *testing.T) {
	state := newElectionState(t)
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	must(t, state.SetLocalRank(federation.RankEntry{CortexID: "01LOCAL", Rank: 20, ObservedAt: stale(now)}))
	must(t, state.SetPeerRank("ai-2", federation.RankEntry{CortexID: "01PEER", Rank: 80, ObservedAt: stale(now)}))

	emitter := &fakeEmitter{}
	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID:  "01LOCAL",
		PeerNames: []string{"ai-2"},
		Now:       func() time.Time { return now },
		State:     state,
		Emitter:   emitter,
	})

	inner := &callTracker{}
	wrapped := consolidation.WithElection(inner.pass, e, nil)

	if err := wrapped(context.Background(), "cron"); err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if inner.calls != 0 {
		t.Errorf("pass calls = %d, want 0 (we lost election)", inner.calls)
	}
	// Observers emit nothing — no Claim, no Success, no Fail. Phase 4
	// may add an observation event if telemetry demands it.
	if emitter.count(event.ActionConsolidationClaim) != 0 {
		t.Errorf("observer emitted claim: count = %d", emitter.count(event.ActionConsolidationClaim))
	}
}

func TestWithElection_RunsWhenElected(t *testing.T) {
	state := newElectionState(t)
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	must(t, state.SetLocalRank(federation.RankEntry{CortexID: "01LOCAL", Rank: 99, ObservedAt: stale(now)}))
	must(t, state.SetPeerRank("ai-2", federation.RankEntry{CortexID: "01PEER", Rank: 10, ObservedAt: stale(now)}))

	emitter := &fakeEmitter{}
	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID:    "01LOCAL",
		PeerNames:   []string{"ai-2"},
		QuietPeriod: 0, // zero = skip the sleep for fast tests
		Now:         func() time.Time { return now },
		State:       state,
		Emitter:     emitter,
	})

	inner := &callTracker{}
	wrapped := consolidation.WithElection(inner.pass, e, nil)

	if err := wrapped(context.Background(), "cron"); err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("pass calls = %d, want 1 (we won election)", inner.calls)
	}
	// Winner emits exactly one Claim and one Success.
	if got, want := emitter.count(event.ActionConsolidationClaim), 1; got != want {
		t.Errorf("claim events = %d, want %d", got, want)
	}
	if got, want := emitter.count(event.ActionConsolidationSuccess), 1; got != want {
		t.Errorf("success events = %d, want %d", got, want)
	}
	if got := emitter.count(event.ActionConsolidationFail); got != 0 {
		t.Errorf("fail events = %d, want 0", got)
	}
}

func TestWithElection_EmitsFailOnPassError(t *testing.T) {
	state := newElectionState(t)
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	must(t, state.SetLocalRank(federation.RankEntry{CortexID: "01LOCAL", Rank: 50, ObservedAt: stale(now)}))

	emitter := &fakeEmitter{}
	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID: "01LOCAL",
		Now:      func() time.Time { return now },
		State:    state,
		Emitter:  emitter,
	})

	passErr := errors.New("LLM backend returned 500")
	inner := &callTracker{err: passErr}
	wrapped := consolidation.WithElection(inner.pass, e, nil)

	err := wrapped(context.Background(), "cron")
	if err == nil || !errors.Is(err, passErr) {
		t.Errorf("error = %v, want wrapping %v", err, passErr)
	}
	if got, want := emitter.count(event.ActionConsolidationFail), 1; got != want {
		t.Errorf("fail events = %d, want %d", got, want)
	}
	if got := emitter.count(event.ActionConsolidationSuccess); got != 0 {
		t.Errorf("success events = %d, want 0 (pass errored)", got)
	}
}

func TestWithElection_HonorsContextCancellationDuringQuietPeriod(t *testing.T) {
	// A context cancelled during the quiet-period sleep should emit
	// Fail and return ctx.Err so Agent.Stop() drains promptly rather
	// than blocking on a multi-second sleep.
	state := newElectionState(t)
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	must(t, state.SetLocalRank(federation.RankEntry{CortexID: "01LOCAL", Rank: 50, ObservedAt: stale(now)}))

	emitter := &fakeEmitter{}
	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID:    "01LOCAL",
		QuietPeriod: 10 * time.Second, // long — we'll cancel before it elapses
		Now:         func() time.Time { return now },
		State:       state,
		Emitter:     emitter,
	})

	inner := &callTracker{}
	wrapped := consolidation.WithElection(inner.pass, e, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the sleep returns immediately

	err := wrapped(ctx, "cron")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if inner.calls != 0 {
		t.Errorf("pass calls = %d, want 0 (cancelled before run)", inner.calls)
	}
	if got := emitter.count(event.ActionConsolidationFail); got != 1 {
		t.Errorf("fail events = %d, want 1 (preempted)", got)
	}
}

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
	wrapped := consolidation.WithElection(inner.pass, e, nil, nil)

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
	wrapped := consolidation.WithElection(inner.pass, e, nil, nil)

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
	wrapped := consolidation.WithElection(inner.pass, e, nil, nil)

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
	// Fail with the context_canceled reason and return ctx.Err so
	// Agent.Stop() drains promptly rather than blocking on a multi-
	// second sleep.
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
	wrapped := consolidation.WithElection(inner.pass, e, nil, nil)

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
		t.Errorf("fail events = %d, want 1 (context canceled)", got)
	}
	if reason := failReason(emitter); reason != consolidation.FailReasonContextCanceled {
		t.Errorf("fail reason = %q, want %q", reason, consolidation.FailReasonContextCanceled)
	}
}

// failReason returns the Reason field of the last ActionConsolidationFail
// event recorded by emitter, or "" if none. Lets the gate tests assert
// the specific preemption sub-reason now that the wrapper distinguishes
// peer-outranked vs. no-winner-at-recheck vs. context-canceled.
func failReason(emitter *fakeEmitter) string {
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	for i := len(emitter.events) - 1; i >= 0; i-- {
		if emitter.events[i].Action != event.ActionConsolidationFail {
			continue
		}
		fd, ok := emitter.events[i].Data.(consolidation.FailData)
		if !ok {
			return ""
		}
		return fd.Reason
	}
	return ""
}

func TestWithElection_EmitsNoWinnerAtRecheck(t *testing.T) {
	// Recheck-stall guard: if every rank entry expires or gets
	// filtered between Decide() and the post-quiet recheck, the
	// gate must surface FailReasonNoWinnerAtRecheck rather than the
	// peer-outranked reason. Operationally this signals "everyone
	// dropped out", not "someone else won" — different debugging path.
	//
	// At t=0 the local rank wins. The quiet-wait hook then demotes it
	// to RankIneligible (0) so the recheck finds no eligible peer. The
	// hook runs synchronously between the two Decide calls — no
	// goroutine, no real sleep — so the demotion can't lose a race
	// against the wait under CI load. See SetQuietWaitHook.
	state := newElectionState(t)
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	must(t, state.SetLocalRank(federation.RankEntry{CortexID: "01LOCAL", Rank: 50, ObservedAt: stale(now)}))

	emitter := &fakeEmitter{}
	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID:    "01LOCAL",
		QuietPeriod: 50 * time.Millisecond,
		Now:         func() time.Time { return now },
		State:       state,
		Emitter:     emitter,
	})
	e.SetQuietWaitHook(func(context.Context, time.Duration) error {
		return state.SetLocalRank(federation.RankEntry{CortexID: "01LOCAL", Rank: 0, ObservedAt: stale(now)})
	})

	inner := &callTracker{}
	wrapped := consolidation.WithElection(inner.pass, e, nil, nil)

	if err := wrapped(context.Background(), "cron"); err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if inner.calls != 0 {
		t.Errorf("pass calls = %d, want 0 (no winner at recheck)", inner.calls)
	}
	if got := emitter.count(event.ActionConsolidationFail); got != 1 {
		t.Errorf("fail events = %d, want 1", got)
	}
	if reason := failReason(emitter); reason != consolidation.FailReasonNoWinnerAtRecheck {
		t.Errorf("fail reason = %q, want %q", reason, consolidation.FailReasonNoWinnerAtRecheck)
	}
}

func TestWithElection_EmitsPeerOutrankedAtRecheck(t *testing.T) {
	// Peer-outranked-at-recheck guard: a peer that wasn't visible at
	// initial Decide arrives during the quiet period and outranks
	// us. The wrapper must surface FailReasonPeerOutranked, not the
	// no-winner reason. The higher-ranked peer is published synchronously
	// from the quiet-wait hook (between the two Decide calls) so the
	// arrival can't race the wait under CI load. See SetQuietWaitHook.
	state := newElectionState(t)
	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	must(t, state.SetLocalRank(federation.RankEntry{CortexID: "01LOCAL", Rank: 30, ObservedAt: stale(now)}))

	emitter := &fakeEmitter{}
	e := consolidation.NewElection(consolidation.ElectionConfig{
		CortexID:    "01LOCAL",
		PeerNames:   []string{"ai-2"},
		QuietPeriod: 50 * time.Millisecond,
		Now:         func() time.Time { return now },
		State:       state,
		Emitter:     emitter,
	})
	e.SetQuietWaitHook(func(context.Context, time.Duration) error {
		return state.SetPeerRank("ai-2", federation.RankEntry{CortexID: "01PEER", Rank: 90, ObservedAt: stale(now)})
	})

	inner := &callTracker{}
	wrapped := consolidation.WithElection(inner.pass, e, nil, nil)

	if err := wrapped(context.Background(), "cron"); err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if inner.calls != 0 {
		t.Errorf("pass calls = %d, want 0 (peer outranked us)", inner.calls)
	}
	if reason := failReason(emitter); reason != consolidation.FailReasonPeerOutranked {
		t.Errorf("fail reason = %q, want %q", reason, consolidation.FailReasonPeerOutranked)
	}
}

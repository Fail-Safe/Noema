package consolidation_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/consolidation"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/event"
)

// buildWatchdogCortex stands up a real cortex on a temp dir so the
// watchdog can query a live events table. Returns the cortex (which
// implements EventEmitter) and a cleanup-bound handle so tests can
// inject claims via the same EmitCoordinationEvent path used in
// production.
func buildWatchdogCortex(t *testing.T) *cortex.Cortex {
	t.Helper()
	dir := t.TempDir()
	if _, err := cortex.Create("watchdog", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("watchdog", filepath.Join(dir, "watchdog"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx
}

// emitClaim is a small helper that pushes a synthetic claim through the
// real EmitCoordinationEvent path. Returns the window ID used so the
// test can assert against it.
func emitClaim(t *testing.T, cx *cortex.Cortex, winnerID string) string {
	t.Helper()
	windowID := event.NewULID()
	if err := cx.EmitCoordinationEvent(
		event.ActionConsolidationClaim, windowID,
		consolidation.ClaimData{WindowID: windowID, CortexID: winnerID},
	); err != nil {
		t.Fatalf("emit claim: %v", err)
	}
	return windowID
}

// countActions returns the number of events with the given action in
// the local log. Used to assert how many fails the watchdog emitted.
func countActions(t *testing.T, cx *cortex.Cortex, action event.Action) int {
	t.Helper()
	events, err := cx.EventsSince("", 1000)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Action == action {
			n++
		}
	}
	return n
}

// findFailReason returns the reason of the (last) fail event for the
// given window ID, or "" if none. Lets us verify the watchdog stamped
// the right reason.
func findFailReason(t *testing.T, cx *cortex.Cortex, windowID string) string {
	t.Helper()
	events, err := cx.EventsSince("", 1000)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Action != event.ActionConsolidationFail || e.TraceID != windowID {
			continue
		}
		// data is JSON: parse the Reason field. We don't decode the
		// full FailData struct here to avoid coupling the test to its
		// exact shape — a substring check is enough.
		body := string(e.Data)
		// Look for "reason":"<value>" in the JSON payload.
		const key = `"reason":"`
		idx := indexOf(body, key)
		if idx < 0 {
			return ""
		}
		rest := body[idx+len(key):]
		end := indexOf(rest, `"`)
		if end < 0 {
			return ""
		}
		return rest[:end]
	}
	return ""
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestWatchdog_StaleClaimEmitsFail(t *testing.T) {
	// Claim emitted now; sweep with a future "now" past the timeout
	// so the claim looks stale. The watchdog should emit exactly one
	// closing fail with reason=watchdog_expired and the silent
	// winner's cortex_id stamped on the FailData payload.
	cx := buildWatchdogCortex(t)
	silentWinner := "01SILENTWINNERPEERIDXXXXXXX"
	windowID := emitClaim(t, cx, silentWinner)

	future := time.Now().Add(20 * time.Minute)
	w := consolidation.NewWatchdog(consolidation.WatchdogConfig{
		DB:            cx.DB.DB,
		Emitter:       cx,
		LocalCortexID: cx.ID,
		Timeout:       10 * time.Minute,
		Now:           func() time.Time { return future },
	})

	if err := w.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if got, want := countActions(t, cx, event.ActionConsolidationFail), 1; got != want {
		t.Errorf("fail events = %d, want %d", got, want)
	}
	if reason := findFailReason(t, cx, windowID); reason != consolidation.FailReasonWatchdogExpired {
		t.Errorf("fail reason = %q, want %q", reason, consolidation.FailReasonWatchdogExpired)
	}
}

func TestWatchdog_RecentClaimSkipped(t *testing.T) {
	// Claim well within the timeout window — the watchdog must leave
	// it alone.
	cx := buildWatchdogCortex(t)
	emitClaim(t, cx, "01PEER")

	// Sweep with now = claim time + 30s; timeout = 10m, so the claim
	// is far from stale.
	near := time.Now().Add(30 * time.Second)
	w := consolidation.NewWatchdog(consolidation.WatchdogConfig{
		DB:      cx.DB.DB,
		Emitter: cx,
		Timeout: 10 * time.Minute,
		Now:     func() time.Time { return near },
	})

	if err := w.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := countActions(t, cx, event.ActionConsolidationFail); got != 0 {
		t.Errorf("fail events = %d, want 0 (claim is recent)", got)
	}
}

func TestWatchdog_ResolvedClaimSkipped(t *testing.T) {
	// A claim with a matching success or fail event already in the log
	// must not be re-closed by the watchdog. The NOT EXISTS clause in
	// findOrphans handles both cases via the IN list.
	cx := buildWatchdogCortex(t)
	winner := "01PEER"

	// Window 1: resolved by success.
	w1 := emitClaim(t, cx, winner)
	if err := cx.EmitCoordinationEvent(
		event.ActionConsolidationSuccess, w1,
		consolidation.SuccessData{WindowID: w1, CortexID: winner},
	); err != nil {
		t.Fatalf("emit success: %v", err)
	}

	// Window 2: resolved by fail.
	w2 := emitClaim(t, cx, winner)
	if err := cx.EmitCoordinationEvent(
		event.ActionConsolidationFail, w2,
		consolidation.FailData{WindowID: w2, CortexID: winner, Reason: consolidation.FailReasonLLMError},
	); err != nil {
		t.Fatalf("emit fail: %v", err)
	}

	failsBefore := countActions(t, cx, event.ActionConsolidationFail)

	future := time.Now().Add(20 * time.Minute)
	w := consolidation.NewWatchdog(consolidation.WatchdogConfig{
		DB:      cx.DB.DB,
		Emitter: cx,
		Timeout: 10 * time.Minute,
		Now:     func() time.Time { return future },
	})

	if err := w.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// failsBefore captured the existing fail; the watchdog must not
	// add another one (would mean it tried to re-close window 2).
	if got, want := countActions(t, cx, event.ActionConsolidationFail), failsBefore; got != want {
		t.Errorf("fail events = %d, want %d (watchdog should skip resolved windows)", got, want)
	}
}

func TestWatchdog_DedupesAcrossSweeps(t *testing.T) {
	// First sweep closes the orphan; the closing fail then satisfies
	// the NOT EXISTS clause for subsequent sweeps, so re-running
	// Sweep must not produce a second fail.
	cx := buildWatchdogCortex(t)
	emitClaim(t, cx, "01PEER")

	future := time.Now().Add(20 * time.Minute)
	w := consolidation.NewWatchdog(consolidation.WatchdogConfig{
		DB:      cx.DB.DB,
		Emitter: cx,
		Timeout: 10 * time.Minute,
		Now:     func() time.Time { return future },
	})

	if err := w.Sweep(); err != nil {
		t.Fatalf("first Sweep: %v", err)
	}
	if err := w.Sweep(); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}

	if got, want := countActions(t, cx, event.ActionConsolidationFail), 1; got != want {
		t.Errorf("fail events after two sweeps = %d, want %d (dedupe broken)", got, want)
	}
}

func TestWatchdog_StartStopLifecycle(t *testing.T) {
	// End-to-end: the loop wakes up, runs an initial Sweep, then
	// drains cleanly when Stop is called. Tight Interval keeps the
	// test fast; the initial Sweep happens before the first tick so
	// we get the orphan closure even with a sub-second runtime.
	cx := buildWatchdogCortex(t)
	emitClaim(t, cx, "01PEER")

	future := time.Now().Add(20 * time.Minute)
	w := consolidation.NewWatchdog(consolidation.WatchdogConfig{
		DB:       cx.DB.DB,
		Emitter:  cx,
		Timeout:  10 * time.Minute,
		Interval: 50 * time.Millisecond,
		Now:      func() time.Time { return future },
	})

	w.Start()
	// Give the initial Sweep + at least one tick a chance to run.
	time.Sleep(150 * time.Millisecond)
	w.Stop()

	if got, want := countActions(t, cx, event.ActionConsolidationFail), 1; got != want {
		t.Errorf("fail events after Start/Stop cycle = %d, want %d", got, want)
	}
}

func TestWatchdog_StopWithoutStartIsSafe(t *testing.T) {
	// Defensive: Stop on a never-Started watchdog must not panic or
	// block forever. Mirrors the same guarantee EligibilityLoop.Stop
	// makes for the same reason.
	cx := buildWatchdogCortex(t)
	w := consolidation.NewWatchdog(consolidation.WatchdogConfig{
		DB:      cx.DB.DB,
		Emitter: cx,
	})
	w.Stop() // should be a no-op
}

package consolidation

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeCortex lets tests drive the cadence inputs without a real DB.
type fakeCortex struct {
	mu             sync.Mutex
	lastEvent      time.Time
	shortN         int
	successAfterAt time.Time // sentinel: HasConsolidationSuccessAfter
	// returns true when its cutoff is strictly < successAfterAt. Zero
	// (default) means "no successes recorded" — every call returns false,
	// which is the worst case the retry path needs to handle and matches
	// the behaviour of a cortex whose pass deferred without emitting.
}

func (f *fakeCortex) LastMutationTime() (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastEvent, nil
}

func (f *fakeCortex) ShortTierCount() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shortN, nil
}

func (f *fakeCortex) HasConsolidationSuccessAfter(cutoff time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.successAfterAt.IsZero() {
		return false, nil
	}
	return f.successAfterAt.After(cutoff), nil
}

func (f *fakeCortex) setShortN(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shortN = n
}

func (f *fakeCortex) setLastEvent(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastEvent = t
}

func (f *fakeCortex) setSuccessAt(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.successAfterAt = t
}

// recorderPass captures each pass invocation so tests can assert
// which trigger fired without racing against goroutine timing.
type recorderPass struct {
	mu       sync.Mutex
	triggers []string
}

func (r *recorderPass) fn(ctx context.Context, trigger string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.triggers = append(r.triggers, trigger)
	return nil
}

func (r *recorderPass) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.triggers)
}

func (r *recorderPass) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.triggers) == 0 {
		return ""
	}
	return r.triggers[len(r.triggers)-1]
}

// testClock is a settable time source for deterministic cadence tests.
// Using a pointer lets test helpers advance the clock without the
// timezone drift that time.Unix/UnixNano imposes on round-trip.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *testClock) get() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// newTestAgent builds an agent with injected clock and pass fn so
// tests can advance time manually and inspect pass invocations.
func newTestAgent(t *testing.T, cfg Config, cx Cortex) (*Agent, *recorderPass, *testClock) {
	t.Helper()
	rec := &recorderPass{}
	clock := &testClock{}
	a := New(cx, cfg, rec.fn, nil)
	a.now = clock.get
	return a, rec, clock
}

// ---- parseCronHHMM ----

func TestParseCronHHMM(t *testing.T) {
	ok := []string{"00:00", "03:00", "23:59", "09:30"}
	for _, s := range ok {
		if _, err := parseCronHHMM(s); err != nil {
			t.Errorf("parseCronHHMM(%q) unexpected error: %v", s, err)
		}
	}
	bad := []string{"", "3:00", "25:00", "03:60", "0300", "three o'clock"}
	for _, s := range bad {
		if _, err := parseCronHHMM(s); err == nil {
			t.Errorf("parseCronHHMM(%q) should have failed", s)
		}
	}
}

// ---- Cron trigger ----

func TestCron_FiresOncePerDay(t *testing.T) {
	// Configure cron for 03:00; walk the clock from before to after.
	cx := &fakeCortex{}
	a, rec, clock := newTestAgent(t, Config{Cron: "03:00"}, cx)

	// 02:59 — not yet.
	clock.set(time.Date(2026, 4, 19, 2, 59, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 0 {
		t.Errorf("cron fired before scheduled time: %d passes", n)
	}

	// 03:00:00 — fire.
	clock.set(time.Date(2026, 4, 19, 3, 0, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 1 {
		t.Fatalf("cron did not fire at scheduled time: %d passes", n)
	}
	if got := rec.last(); got != "cron" {
		t.Errorf("trigger = %q, want %q", got, "cron")
	}

	// 03:05 same day — must NOT re-fire.
	clock.set(time.Date(2026, 4, 19, 3, 5, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 1 {
		t.Errorf("cron re-fired same day: %d passes", n)
	}

	// 03:00 next day — fire again.
	clock.set(time.Date(2026, 4, 20, 3, 0, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 2 {
		t.Errorf("cron did not fire on next day: %d passes", n)
	}
}

func TestCron_DisabledWhenEmpty(t *testing.T) {
	cx := &fakeCortex{}
	a, rec, clock := newTestAgent(t, Config{}, cx)
	clock.set(time.Date(2026, 4, 19, 3, 0, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 0 {
		t.Errorf("no-trigger config fired pass: %d", n)
	}
}

// ---- Threshold trigger ----

func TestThreshold_FiresAboveLimit_ThenHysteresis(t *testing.T) {
	cx := &fakeCortex{}
	a, rec, clock := newTestAgent(t, Config{ThresholdShort: 100}, cx)
	clock.set(time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC))

	// Below threshold: no fire.
	cx.setShortN(50)
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 0 {
		t.Errorf("fired below threshold: %d", n)
	}

	// Cross threshold: fire.
	cx.setShortN(101)
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 1 {
		t.Fatalf("did not fire at threshold crossing: %d", n)
	}
	if got := rec.last(); got != "threshold" {
		t.Errorf("trigger = %q, want threshold", got)
	}

	// Still above: must NOT re-fire (armed state).
	cx.setShortN(105)
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 1 {
		t.Errorf("re-fired while still armed: %d", n)
	}

	// Drops a little (95 > 0.8 * 100 = 80): still armed.
	cx.setShortN(95)
	a.evaluateAndMaybeRun()
	cx.setShortN(101)
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 1 {
		t.Errorf("re-fired without sufficient hysteresis: %d", n)
	}

	// Drops below 80 (75 < 80): re-arm.
	cx.setShortN(75)
	a.evaluateAndMaybeRun()
	// Re-crosses threshold: should fire again.
	cx.setShortN(101)
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 2 {
		t.Errorf("did not re-fire after hysteresis reset: %d", n)
	}
}

// ---- Idle trigger ----

func TestIdle_FiresAfterCooldown(t *testing.T) {
	cx := &fakeCortex{}
	a, rec, clock := newTestAgent(t, Config{IdleMinutes: 30}, cx)
	now := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)

	// Simulate a mutation at noon, then advance clock 29 minutes — not yet idle enough.
	cx.setLastEvent(now)
	clock.set(now.Add(29 * time.Minute))
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 0 {
		t.Errorf("idle fired before cooldown: %d", n)
	}

	// 30 minutes since last mutation: fire.
	clock.set(now.Add(30 * time.Minute))
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 1 {
		t.Fatalf("idle did not fire at cooldown: %d", n)
	}
	if got := rec.last(); got != "idle" {
		t.Errorf("trigger = %q, want idle", got)
	}

	// Another 29 minutes (total 59m since last mutation, 29m since last run):
	// NOT enough for cooldown to elapse.
	clock.set(now.Add(59 * time.Minute))
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 1 {
		t.Errorf("idle re-fired within cooldown: %d", n)
	}

	// Another minute (60m since last mutation, 30m since last run): re-fire.
	clock.set(now.Add(60 * time.Minute))
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 2 {
		t.Errorf("idle did not re-fire after cooldown elapsed: %d", n)
	}
}

func TestIdle_EmptyEventLogDoesNotFire(t *testing.T) {
	// A fresh cortex with no events should not trigger idle even though
	// the "last mutation" time is effectively infinity ago. Otherwise
	// every cortex would fire a pass on first startup.
	cx := &fakeCortex{}
	a, rec, clock := newTestAgent(t, Config{IdleMinutes: 30}, cx)
	clock.set(time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()
	if n := rec.count(); n != 0 {
		t.Errorf("idle fired on empty event log: %d", n)
	}
}

// ---- Priority and composition ----

func TestTriggerPriority_CronBeforeThreshold(t *testing.T) {
	// When both cron and threshold want to fire, cron wins so the
	// trigger label in the event log reflects the most predictable
	// cause.
	cx := &fakeCortex{shortN: 200}
	a, rec, clock := newTestAgent(t, Config{Cron: "03:00", ThresholdShort: 100}, cx)
	clock.set(time.Date(2026, 4, 19, 3, 0, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()
	if got := rec.last(); got != "cron" {
		t.Errorf("priority: trigger = %q, want cron", got)
	}
}

// ---- Lifecycle ----

func TestStartStop_CleanShutdown(t *testing.T) {
	cx := &fakeCortex{}
	a := New(cx, Config{Cron: "00:00", PollInterval: 10 * time.Millisecond}, nil, nil)
	a.Start()
	time.Sleep(30 * time.Millisecond) // let the loop tick at least twice
	a.Stop()                          // must return promptly, no hang
}

func TestStopWithoutStart_NoPanic(t *testing.T) {
	cx := &fakeCortex{}
	a := New(cx, Config{}, nil, nil)
	a.Stop() // should be a no-op, not a panic
}

// ---- Cron retry-on-idle (issue #56) ----

// retryConfig is the canonical config used by the cron-retry tests:
// 5-minute retry window, 3 retries. Tight enough to drive multiple
// retries inside a test by stepping the clock, loose enough to mirror
// realistic production defaults.
func retryConfig() Config {
	return Config{
		Cron:            "03:00",
		CronRetryWindow: 5 * time.Minute,
		CronMaxRetries:  3,
	}
}

func TestCronRetry_DefersMarkUntilSuccess(t *testing.T) {
	// With retries enabled, a cron fire must not mark lastCronDay
	// until checkCronRetry observes a success event. cronTriggered
	// must also stay quiet during the awaiting window so the regular
	// trigger path can't double-fire.
	cx := &fakeCortex{}
	a, rec, clock := newTestAgent(t, retryConfig(), cx)

	// 03:00 — first fire.
	clock.set(time.Date(2026, 4, 19, 3, 0, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()
	if rec.count() != 1 {
		t.Fatalf("first fire = %d, want 1", rec.count())
	}

	// 03:01 — within retry window, no success yet. The trigger
	// path must not re-fire (awaiting state guards it) and the
	// retry check is too early to act.
	clock.set(time.Date(2026, 4, 19, 3, 1, 0, 0, time.UTC))
	a.checkCronRetry()
	a.evaluateAndMaybeRun()
	if rec.count() != 1 {
		t.Errorf("re-fired during awaiting window: count = %d", rec.count())
	}
}

func TestCronRetry_SuccessClearsState(t *testing.T) {
	// A success event landing during the retry window clears the
	// awaiting state and marks lastCronDay so cron stops trying
	// today and the agent goes back to its normal cadence.
	cx := &fakeCortex{}
	a, rec, clock := newTestAgent(t, retryConfig(), cx)

	clock.set(time.Date(2026, 4, 19, 3, 0, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()
	if rec.count() != 1 {
		t.Fatalf("first fire = %d, want 1", rec.count())
	}

	// Simulate a success event arriving from a peer (or our own
	// pass) at 03:02.
	cx.setSuccessAt(time.Date(2026, 4, 19, 3, 2, 0, 0, time.UTC))

	// 03:06 — first retry check past the window, success is found.
	clock.set(time.Date(2026, 4, 19, 3, 6, 0, 0, time.UTC))
	a.checkCronRetry()
	if rec.count() != 1 {
		t.Errorf("retry fired after success: count = %d", rec.count())
	}

	// 03:07 — trigger evaluation must now skip cron because
	// lastCronDay is set.
	clock.set(time.Date(2026, 4, 19, 3, 7, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()
	if rec.count() != 1 {
		t.Errorf("cron re-fired after success: count = %d", rec.count())
	}
}

func TestCronRetry_RefiresAfterWindow(t *testing.T) {
	// No success event lands within the retry window, so each
	// successive retry check past the window re-fires the pass
	// until retries are exhausted.
	cx := &fakeCortex{}
	cfg := retryConfig() // 3 retries, 5-min window
	a, rec, clock := newTestAgent(t, cfg, cx)

	// 03:00 — first fire.
	clock.set(time.Date(2026, 4, 19, 3, 0, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()
	if rec.count() != 1 {
		t.Fatalf("first fire = %d, want 1", rec.count())
	}

	// 03:06 — retry #1 (1st of 3 retries fires).
	clock.set(time.Date(2026, 4, 19, 3, 6, 0, 0, time.UTC))
	a.checkCronRetry()
	if rec.count() != 2 {
		t.Errorf("retry 1 count = %d, want 2", rec.count())
	}

	// 03:12 — retry #2.
	clock.set(time.Date(2026, 4, 19, 3, 12, 0, 0, time.UTC))
	a.checkCronRetry()
	if rec.count() != 3 {
		t.Errorf("retry 2 count = %d, want 3", rec.count())
	}

	// 03:18 — retry #3 (last available).
	clock.set(time.Date(2026, 4, 19, 3, 18, 0, 0, time.UTC))
	a.checkCronRetry()
	if rec.count() != 4 {
		t.Errorf("retry 3 count = %d, want 4", rec.count())
	}
}

func TestCronRetry_ExhaustedMarksDay(t *testing.T) {
	// After the configured retry budget is spent without a success
	// event, the agent gives up and marks lastCronDay so cron sleeps
	// until tomorrow rather than looping forever.
	cx := &fakeCortex{}
	cfg := retryConfig()
	a, rec, clock := newTestAgent(t, cfg, cx)

	// First fire + 3 retries = 4 total invocations.
	clock.set(time.Date(2026, 4, 19, 3, 0, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()

	for i := 1; i <= 3; i++ {
		clock.set(time.Date(2026, 4, 19, 3, i*6, 0, 0, time.UTC))
		a.checkCronRetry()
	}
	if rec.count() != 4 {
		t.Fatalf("total fires after retries = %d, want 4", rec.count())
	}

	// 03:24 — fourth check; retries are now exhausted, so this
	// must NOT fire again. lastCronDay should be set so cron
	// also doesn't fire from the normal trigger path today.
	clock.set(time.Date(2026, 4, 19, 3, 24, 0, 0, time.UTC))
	a.checkCronRetry()
	a.evaluateAndMaybeRun()
	if rec.count() != 4 {
		t.Errorf("fired after retry exhaustion: count = %d", rec.count())
	}

	// Tomorrow at 03:00 — cron must fire again, the day mark
	// only suppresses cron for the day it was set.
	clock.set(time.Date(2026, 4, 20, 3, 0, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()
	if rec.count() != 5 {
		t.Errorf("cron did not fire next day: count = %d", rec.count())
	}
}

func TestCronRetry_SuccessFromAnyPeerCounts(t *testing.T) {
	// HasConsolidationSuccessAfter is the source of truth for
	// "did a pass happen". A success event with a remote cortex_id
	// (replayed via federation) satisfies the local retry check
	// just as well as a local success — that's the point of the
	// retry path. We model this with the fake cortex's
	// successAfterAt timestamp without caring about origin.
	cx := &fakeCortex{}
	a, rec, clock := newTestAgent(t, retryConfig(), cx)

	clock.set(time.Date(2026, 4, 19, 3, 0, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()

	// Peer ran the pass and we replayed a success at 03:03.
	cx.setSuccessAt(time.Date(2026, 4, 19, 3, 3, 0, 0, time.UTC))

	// 03:06 — retry check finds the peer's success and stops here.
	clock.set(time.Date(2026, 4, 19, 3, 6, 0, 0, time.UTC))
	a.checkCronRetry()
	if rec.count() != 1 {
		t.Errorf("retried despite peer success: count = %d", rec.count())
	}
}

func TestCronRetry_DisabledWhenWindowZero(t *testing.T) {
	// Single-node cortexes (whose passes don't emit success events)
	// and tests that don't want to model the retry path opt out by
	// leaving CronRetryWindow at zero. In that case the historical
	// "mark day done immediately" behaviour applies — exactly one
	// fire per day, no retries, no awaiting state.
	cx := &fakeCortex{}
	a, rec, clock := newTestAgent(t, Config{Cron: "03:00"}, cx)

	clock.set(time.Date(2026, 4, 19, 3, 0, 0, 0, time.UTC))
	a.evaluateAndMaybeRun()
	if rec.count() != 1 {
		t.Fatalf("first fire = %d, want 1", rec.count())
	}

	// checkCronRetry must be a no-op when retry is disabled even
	// though we're past where a retry window would have been.
	clock.set(time.Date(2026, 4, 19, 3, 30, 0, 0, time.UTC))
	a.checkCronRetry()
	a.evaluateAndMaybeRun()
	if rec.count() != 1 {
		t.Errorf("retry-disabled fired again: count = %d", rec.count())
	}
}

// Package consolidation schedules and executes memory-tier consolidation
// passes inside `noema serve`. See docs/plans/consolidation-plan.md
// §4 in the Noema-design repo for the design. Phase 7 ships the
// infrastructure (agent lifecycle + cadence composition). The pass
// function itself is injected so later phases can populate it with
// candidate selection (Phase 8) and LLM distillation (Phase 9)
// without touching this package.
package consolidation

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Cortex is the subset of the cortex.Cortex surface the agent needs.
// Keeping it narrow lets tests fake idle time and tier counts without
// constructing a real cortex.
type Cortex interface {
	LastMutationTime() (time.Time, error)
	ShortTierCount() (int, error)
	// HasConsolidationSuccessAfter is queried by the cron retry path
	// to detect whether a fired trigger actually resulted in a pass
	// running (locally or on any peer that replayed a success event
	// back to us via federation). See checkCronRetry for the protocol.
	HasConsolidationSuccessAfter(cutoff time.Time) (bool, error)
}

// PassFn is the consolidation work itself. trigger identifies which
// cadence caused this pass (one of "cron", "idle", "threshold") so
// later-phase implementations can log / telemeter differently. The
// function must honor ctx for shutdown.
type PassFn func(ctx context.Context, trigger string) error

// Config controls the agent's scheduling. All three triggers are
// composable; the agent fires on whichever activates first.
type Config struct {
	// Cron is the nightly trigger time in "HH:MM" (local clock). Empty
	// disables the cron trigger.
	Cron string
	// IdleMinutes fires a pass when no mutation has hit the event log
	// for this many minutes. Zero disables.
	IdleMinutes int
	// ThresholdShort fires a pass when the short-term tier count
	// exceeds this. Zero disables.
	ThresholdShort int
	// PollInterval controls how often the cadence state is re-evaluated.
	// Zero defaults to 60s — tight enough that cron fires within a
	// minute of the configured time, loose enough that the agent is
	// effectively free of background cost.
	PollInterval time.Duration
	// CronRetryWindow is how long to wait after a cron trigger fires
	// before checking whether it actually resulted in a consolidation
	// pass running (locally or on a peer). If no success event has
	// landed within this window, the trigger re-fires up to
	// CronMaxRetries times. Zero disables retries entirely — useful for
	// single-node cortexes whose passes don't emit success events, and
	// for tests that drive evaluateAndMaybeRun directly. Issue #56:
	// without this, a staggered-restart election can have every peer
	// defer to another, nobody runs, and the cron opportunity is burned
	// until tomorrow.
	CronRetryWindow time.Duration
	// CronMaxRetries bounds how many times a single cron fire will
	// re-attempt before the agent gives up and marks the day as failed.
	// Zero treated as "no retries", same as CronRetryWindow=0. Defaults
	// applied in cmd_serve, not here, so tests can probe the zero case.
	CronMaxRetries int
}

// Agent runs cadence evaluation and dispatches passes to the PassFn.
// One Agent per cortex. Safe for concurrent Start/Stop, not safe to
// share across cortexes.
type Agent struct {
	cx   Cortex
	cfg  Config
	pass PassFn
	log  func(format string, args ...any)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	now    func() time.Time

	mu             sync.Mutex
	lastRun        time.Time // most recent pass fire time (any trigger)
	thresholdArmed bool      // true while short count > threshold
	lastCronDay    string    // date of most recent successfully-completed
	// cron pass (YYYY-MM-DD). Set after a consolidation_success has been
	// observed for the trigger fire (or after retries are exhausted, so
	// we don't loop the same day forever). Until then cron is allowed to
	// re-fire — this is what closes the issue #56 split-brain hole.

	// Cron retry state. cronAwaiting is true between the moment a cron
	// trigger fires and the moment we either observe a success event or
	// give up on retries. cronFireTime is the most recent fire time used
	// as the cutoff for HasConsolidationSuccessAfter; it advances on
	// each retry so the success check ignores events older than the
	// current attempt.
	cronAwaiting    bool
	cronFireTime    time.Time
	cronRetriesLeft int
	cronTargetDay   string
}

// New constructs an agent. Call Start to begin the loop. Passing a nil
// PassFn is allowed (the agent logs and returns without doing work);
// early phases use that to exercise cadence code in isolation.
func New(cx Cortex, cfg Config, pass PassFn, log func(format string, args ...any)) *Agent {
	if log == nil {
		log = func(string, ...any) {}
	}
	if pass == nil {
		pass = func(context.Context, string) error { return nil }
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 60 * time.Second
	}
	return &Agent{
		cx:   cx,
		cfg:  cfg,
		pass: pass,
		log:  log,
		now:  time.Now,
	}
}

// Start kicks off the background loop. Call exactly once per Agent;
// subsequent calls after Stop require a fresh New.
func (a *Agent) Start() {
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.wg.Add(1)
	go a.loop()
}

// Stop signals the loop to exit and blocks until it does. A pass in
// flight is allowed to finish; new passes will not start. Safe to call
// even if Start never ran (no-op).
func (a *Agent) Stop() {
	if a.cancel == nil {
		return
	}
	a.cancel()
	a.wg.Wait()
}

func (a *Agent) loop() {
	defer a.wg.Done()
	ticker := time.NewTicker(a.cfg.PollInterval)
	defer ticker.Stop()

	// Check once on startup so cron / threshold triggers that are
	// already due don't wait a full poll interval for their first fire.
	a.checkCronRetry()
	a.evaluateAndMaybeRun()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.checkCronRetry()
			a.evaluateAndMaybeRun()
		}
	}
}

func (a *Agent) evaluateAndMaybeRun() {
	trigger := a.whichTrigger()
	if trigger == "" {
		return
	}
	now := a.now()
	a.mu.Lock()
	a.lastRun = now
	if trigger == "cron" {
		// Don't mark lastCronDay yet — we'll mark it from
		// checkCronRetry once a consolidation_success event has
		// landed (locally or replayed from a peer), or once retries
		// are exhausted. Until then cron is allowed to re-fire from
		// checkCronRetry's retry path.
		if a.cfg.CronRetryWindow > 0 && a.cfg.CronMaxRetries > 0 {
			a.cronAwaiting = true
			a.cronFireTime = now
			a.cronRetriesLeft = a.cfg.CronMaxRetries
			a.cronTargetDay = now.Format("2006-01-02")
		} else {
			// Retry disabled — fall back to the historical
			// "mark day done immediately" behaviour. Used in
			// single-node cortexes (whose passes don't emit
			// success events) and in tests that don't want to
			// model the retry path.
			a.lastCronDay = now.Format("2006-01-02")
		}
	}
	a.mu.Unlock()

	a.log("[consolidation] pass firing (trigger=%s)", trigger)
	if err := a.pass(a.ctx, trigger); err != nil {
		a.log("[consolidation] pass error (trigger=%s): %v", trigger, err)
	}
}

// checkCronRetry runs at the start of every loop tick (after the
// initial fire on startup). When a cron trigger is awaiting its
// success event, this method either:
//
//   - clears the waiting state if a success has landed (we won, a peer
//     ran the pass and we replayed it, or an idle/threshold trigger
//     happened to consolidate the same window before our retry);
//   - re-fires the cron pass if the retry window has elapsed and we
//     still have retries left;
//   - or marks the day as failed (sets lastCronDay so cron sleeps
//     until tomorrow) if retries are exhausted.
//
// All three states converge to "we either succeeded or we'll wait
// until tomorrow" — bounded retries cap the worst-case noise from a
// genuinely sick ring.
//
// The pass invocation happens outside the agent's mutex (mirrors
// evaluateAndMaybeRun's pattern) so a long-running pass doesn't
// block status reads from other goroutines.
func (a *Agent) checkCronRetry() {
	a.mu.Lock()
	if !a.cronAwaiting {
		a.mu.Unlock()
		return
	}
	now := a.now()
	if now.Sub(a.cronFireTime) < a.cfg.CronRetryWindow {
		a.mu.Unlock()
		return
	}
	cutoff := a.cronFireTime
	targetDay := a.cronTargetDay
	a.mu.Unlock()

	// HasConsolidationSuccessAfter is the source of truth: any peer's
	// success event since the last fire counts, since federation
	// replay surfaces remote successes in our local log. Errors here
	// are non-fatal — log and treat as "no success yet" so the next
	// tick retries the check.
	found, err := a.cx.HasConsolidationSuccessAfter(cutoff)
	if err != nil {
		a.log("[consolidation] cron retry check failed for day=%s: %v",
			targetDay, err)
		return
	}

	a.mu.Lock()
	if found {
		a.cronAwaiting = false
		a.lastCronDay = targetDay
		a.mu.Unlock()
		a.log("[consolidation] cron pass succeeded for day=%s", targetDay)
		return
	}
	if a.cronRetriesLeft <= 0 {
		a.cronAwaiting = false
		a.lastCronDay = targetDay // give up for today
		a.mu.Unlock()
		a.log("[consolidation] cron pass exhausted retries for day=%s; giving up until tomorrow",
			targetDay)
		return
	}
	a.cronRetriesLeft--
	a.cronFireTime = now
	a.lastRun = now
	retriesLeft := a.cronRetriesLeft
	a.mu.Unlock()

	a.log("[consolidation] cron pass retry firing (retries_left=%d trigger=cron)",
		retriesLeft)
	if err := a.pass(a.ctx, "cron"); err != nil {
		a.log("[consolidation] cron retry pass error: %v", err)
	}
}

// whichTrigger returns the name of the first trigger that wants to
// fire, or "" if none do. Priority: cron first (most predictable),
// then threshold (bounded growth), then idle (catch-all).
func (a *Agent) whichTrigger() string {
	if a.cronTriggered() {
		return "cron"
	}
	if a.thresholdTriggered() {
		return "threshold"
	}
	if a.idleTriggered() {
		return "idle"
	}
	return ""
}

func (a *Agent) cronTriggered() bool {
	if a.cfg.Cron == "" {
		return false
	}
	scheduled, err := parseCronHHMM(a.cfg.Cron)
	if err != nil {
		return false
	}
	now := a.now()
	today := now.Format("2006-01-02")

	a.mu.Lock()
	alreadyFiredToday := a.lastCronDay == today
	awaitingThisDay := a.cronAwaiting && a.cronTargetDay == today
	a.mu.Unlock()
	if alreadyFiredToday || awaitingThisDay {
		// Awaiting state means cron has already fired and we're
		// either watching for the success event or scheduling a
		// retry via checkCronRetry. The trigger path must stay out
		// of this window; only checkCronRetry is allowed to re-fire
		// cron, and only after CronRetryWindow has elapsed.
		return false
	}
	// Fire when the local clock has passed today's scheduled HH:MM.
	scheduledToday := time.Date(now.Year(), now.Month(), now.Day(),
		scheduled.Hour(), scheduled.Minute(), 0, 0, now.Location())
	return !now.Before(scheduledToday)
}

func (a *Agent) thresholdTriggered() bool {
	if a.cfg.ThresholdShort <= 0 {
		return false
	}
	count, err := a.cx.ShortTierCount()
	if err != nil {
		a.log("[consolidation] threshold probe failed: %v", err)
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// Hysteresis: re-arm only when the count drops below 80% of
	// threshold, else a cortex hovering near the line would thrash.
	if a.thresholdArmed {
		if count < int(float64(a.cfg.ThresholdShort)*0.8) {
			a.thresholdArmed = false
		}
		return false
	}
	if count > a.cfg.ThresholdShort {
		a.thresholdArmed = true
		return true
	}
	return false
}

func (a *Agent) idleTriggered() bool {
	if a.cfg.IdleMinutes <= 0 {
		return false
	}
	last, err := a.cx.LastMutationTime()
	if err != nil {
		a.log("[consolidation] idle probe failed: %v", err)
		return false
	}
	now := a.now()
	// An empty event log (zero time) should not count as "idle forever";
	// require at least one recorded mutation before the idle trigger
	// becomes eligible.
	if last.IsZero() {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// Cooldown: once an idle pass fires, don't re-fire on idle until
	// at least IdleMinutes has elapsed since the last run (any trigger).
	cooldown := time.Duration(a.cfg.IdleMinutes) * time.Minute
	if !a.lastRun.IsZero() && now.Sub(a.lastRun) < cooldown {
		return false
	}
	return now.Sub(last) >= cooldown
}

// parseCronHHMM accepts "HH:MM" (24-hour, two-digit parts) and returns a
// time.Time with the date set to year zero (date fields are not used;
// only Hour/Minute are read by cronTriggered). The strict 5-character
// length check is load-bearing: time.Parse tolerates "3:00" as 03:00,
// and we want typos in cortex.md to surface as errors rather than
// silently running at unexpected times.
func parseCronHHMM(s string) (time.Time, error) {
	if len(s) != 5 || s[2] != ':' {
		return time.Time{}, fmt.Errorf("cron must be HH:MM (e.g. 03:00), got %q", s)
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("cron must be HH:MM (e.g. 03:00), got %q", s)
	}
	return t, nil
}

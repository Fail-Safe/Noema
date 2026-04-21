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
	lastCronDay    string    // date of most recent cron fire (YYYY-MM-DD) so
	// we don't double-fire cron when the clock is near the scheduled time
	// and the poll ticks more than once within the minute.
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
	a.evaluateAndMaybeRun()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.evaluateAndMaybeRun()
		}
	}
}

func (a *Agent) evaluateAndMaybeRun() {
	trigger := a.whichTrigger()
	if trigger == "" {
		return
	}
	a.mu.Lock()
	a.lastRun = a.now()
	if trigger == "cron" {
		a.lastCronDay = a.now().Format("2006-01-02")
	}
	a.mu.Unlock()

	a.log("[consolidation] pass firing (trigger=%s)", trigger)
	if err := a.pass(a.ctx, trigger); err != nil {
		a.log("[consolidation] pass error (trigger=%s): %v", trigger, err)
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
	a.mu.Unlock()
	if alreadyFiredToday {
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

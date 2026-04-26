package consolidation

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/Fail-Safe/Noema/internal/event"
)

// Watchdog defaults — see WatchdogConfig fields for the rationale and
// the consolidation-plan §14 watchdog deferral notes.
const (
	defaultWatchdogTimeout  = 10 * time.Minute
	defaultWatchdogInterval = 1 * time.Minute
)

// WatchdogConfig is the runtime input for a Watchdog instance. Built in
// cmd_serve from the Cortex handle and the federation config.
type WatchdogConfig struct {
	// DB is the cortex's SQLite connection. Required; the sweep query
	// joins the events table to itself to find orphaned claims.
	DB *sql.DB

	// Emitter is the target for the closing fail events. In production
	// this is the same *cortex.Cortex that EligibilityLoop and Election
	// share; the narrow interface lets tests substitute a recorder.
	Emitter EventEmitter

	// LocalCortexID is this cortex's stable ULID. Stamped into every
	// emitted fail event so observers can attribute the closing
	// decision to a specific peer for audit purposes.
	LocalCortexID string

	// Timeout is how old a claim must be (now - claim.timestamp) before
	// the watchdog treats it as orphaned. Generous by default — the
	// longest legitimate pass we expect is LLM distillation on a
	// large window, which still completes well inside this budget.
	// Zero defaults to defaultWatchdogTimeout.
	Timeout time.Duration

	// Interval is the sweep cadence. Sweeps are cheap (one indexed
	// query) so a tight cadence is fine; the trade-off is mostly how
	// long an actually-stuck pass blocks the ring before being
	// closed out. Zero defaults to defaultWatchdogInterval.
	Interval time.Duration

	// Now is injected for tests; zero defaults to time.Now.
	Now func() time.Time

	// Log is the optional logger; nil is a safe no-op.
	Log func(format string, args ...any)
}

// Watchdog scans the local event log for consolidation_claim events with
// no matching success/fail and emits a closing consolidation_fail event
// (reason=watchdog_expired) so the next election cycle can move on. One
// per cortex; lifecycle parallel to Agent + EligibilityLoop.
//
// The watchdog runs on every peer, not just election winners. The whole
// point is that the winner is presumed dead — observers need to notice
// and break out. Cross-peer duplicate emissions are possible (two
// observers detect the same stale claim within seconds of each other);
// events have unique IDs so replay is idempotent and the only cost is a
// little log noise. Cross-peer locking would be substantially more
// complex and isn't worth it for what is already a corner case.
type Watchdog struct {
	cfg WatchdogConfig

	mu     sync.Mutex // serializes Sweep against itself
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewWatchdog constructs a Watchdog with defaults filled in. Call Start
// to begin the sweep loop; call Stop to drain.
func NewWatchdog(cfg WatchdogConfig) *Watchdog {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultWatchdogTimeout
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultWatchdogInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &Watchdog{cfg: cfg}
}

// Start kicks off the background sweep loop. Call exactly once per
// instance; subsequent Start calls after Stop require a fresh
// NewWatchdog.
func (w *Watchdog) Start() {
	w.ctx, w.cancel = context.WithCancel(context.Background())
	w.wg.Add(1)
	go w.loop()
}

// Stop signals the loop to exit and blocks until it does. Safe to call
// even if Start never ran.
func (w *Watchdog) Stop() {
	if w.cancel == nil {
		return
	}
	w.cancel()
	w.wg.Wait()
}

func (w *Watchdog) loop() {
	defer w.wg.Done()

	// Run an initial sweep so a process that was restarted with stale
	// claims in its log doesn't have to wait a full Interval before
	// closing them out.
	if err := w.Sweep(); err != nil {
		w.cfg.Log("[watchdog] initial sweep failed: %v", err)
	}

	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			if err := w.Sweep(); err != nil {
				w.cfg.Log("[watchdog] sweep failed: %v", err)
			}
		}
	}
}

// orphanedClaim is the subset of a claim event needed to emit a closing
// fail. trace_id holds the synthetic window ID for coordination events.
type orphanedClaim struct {
	WindowID  string
	WinnerID  string // cortex_id of the peer that emitted the claim
	Timestamp string
}

// Sweep runs one scan synchronously. Exported for tests and for callers
// that want to force a check without waiting for the next tick.
//
// Not safe for concurrent invocation — the loop goroutine is the only
// expected caller in production. The internal mutex serializes any
// stray test calls against the loop.
func (w *Watchdog) Sweep() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	cutoff := w.cfg.Now().Add(-w.cfg.Timeout).UTC().Format(time.RFC3339)
	orphans, err := w.findOrphans(cutoff)
	if err != nil {
		return fmt.Errorf("watchdog query: %w", err)
	}

	for _, o := range orphans {
		// Each emission goes through the same EmitCoordinationEvent
		// path used by Election, so the closing fail lands in the
		// event log with the local cortex as the emitter and the
		// silent winner's ID embedded in FailData.CortexID. Future
		// sweeps see the new fail via the NOT EXISTS clause and
		// skip the window naturally.
		fd := FailData{
			WindowID: o.WindowID,
			CortexID: o.WinnerID,
			Reason:   FailReasonWatchdogExpired,
		}
		if err := w.cfg.Emitter.EmitCoordinationEvent(
			event.ActionConsolidationFail, o.WindowID, fd,
		); err != nil {
			// Log + continue; one bad emission shouldn't block
			// the rest of the sweep. The next tick will retry
			// any windows we couldn't close this round.
			w.cfg.Log("[watchdog] emit fail for window=%s winner=%s: %v",
				o.WindowID, o.WinnerID, err)
			continue
		}
		w.cfg.Log("[watchdog] closed orphaned claim window=%s winner=%s claimed=%s",
			o.WindowID, o.WinnerID, o.Timestamp)
	}
	return nil
}

// findOrphans returns claim events older than cutoff (RFC3339) that
// have no matching success or fail event in the local log. The query
// is the join described in the watchdog design — a NOT EXISTS subquery
// on the same table keyed on trace_id (= window_id for coordination
// events) means a single ANY-emitted closing event is enough to retire
// the window from future sweeps, naturally deduping across this peer's
// own retries and against fail events replayed from other peers.
func (w *Watchdog) findOrphans(cutoff string) ([]orphanedClaim, error) {
	const q = `
SELECT trace_id, cortex_id, timestamp
FROM events
WHERE action = ?
  AND timestamp < ?
  AND NOT EXISTS (
      SELECT 1 FROM events e2
      WHERE e2.trace_id = events.trace_id
        AND e2.action IN (?, ?)
  )
ORDER BY timestamp`

	rows, err := w.cfg.DB.Query(q,
		string(event.ActionConsolidationClaim),
		cutoff,
		string(event.ActionConsolidationSuccess),
		string(event.ActionConsolidationFail),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []orphanedClaim
	for rows.Next() {
		var o orphanedClaim
		if err := rows.Scan(&o.WindowID, &o.WinnerID, &o.Timestamp); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

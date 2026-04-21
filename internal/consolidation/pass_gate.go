package consolidation

import (
	"context"
	"fmt"
	"time"

	"github.com/Fail-Safe/Noema/internal/event"
)

// WithElection wraps a PassFn so only the elected peer runs it. The
// wrapper implements the happy-path protocol from consolidation-plan.md
// §14; watchdog recovery for a winner that fails to report is deferred
// to a later phase.
//
// Flow:
//
//  1. Decide winner. If we didn't win, log + return nil (observers
//     silently skip).
//  2. Emit ActionConsolidationClaim with a fresh window ID.
//  3. Sleep one QuietPeriod so any parallel-starting claimant has time
//     to surface in federation_state.
//  4. Re-decide. If we no longer win (a higher-tiebreak peer also
//     claimed), emit Fail(aborted_by_peer_conflict) and return.
//  5. Invoke inner(ctx, trigger). On error, emit Fail(reason=error
//     message); on success, emit Success.
//
// Context cancellation during the quiet-period sleep is treated as a
// preemption: the gate emits Fail and returns ctx.Err so the agent's
// Stop() drains promptly.
//
// On a single-node cortex with no peers advertising rank, Decide
// returns ShouldRun=true for the local cortex trivially and the
// wrapper degenerates to "emit Claim, brief sleep, emit Success
// around the pass" — correct but mildly wasteful. Phase 4 can add a
// no-peers fast path if the overhead ever matters.
func WithElection(inner PassFn, election *Election, log func(format string, args ...any)) PassFn {
	if log == nil {
		log = func(string, ...any) {}
	}
	return func(ctx context.Context, trigger string) error {
		initial := election.Decide()
		if !initial.ShouldRun {
			log("[consolidation] skipping pass (trigger=%s): %s", trigger, initial.Reason)
			return nil
		}

		windowID := event.NewULID()
		if err := election.Claim(windowID); err != nil {
			return fmt.Errorf("emitting claim: %w", err)
		}

		// Quiet-period wait for conflicting claims. Respect ctx so
		// Agent.Stop() doesn't block on a multi-second sleep.
		if election.QuietPeriod() > 0 {
			select {
			case <-ctx.Done():
				_ = election.Fail(windowID, FailReasonPreempted)
				return ctx.Err()
			case <-time.After(election.QuietPeriod()):
			}
		}

		recheck := election.Decide()
		if !recheck.ShouldRun || recheck.Winner != election.CortexID() {
			log("[consolidation] preempted during quiet period (trigger=%s): %s",
				trigger, recheck.Reason)
			_ = election.Fail(windowID, FailReasonPreempted)
			return nil
		}

		log("[consolidation] running as elected winner (trigger=%s window=%s)",
			trigger, windowID)
		if err := inner(ctx, trigger); err != nil {
			_ = election.Fail(windowID, err.Error())
			return err
		}
		if err := election.Success(windowID, 0, 0); err != nil {
			log("[consolidation] success event emission failed: %v", err)
		}
		return nil
	}
}

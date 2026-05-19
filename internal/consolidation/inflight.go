package consolidation

import "sync"

// InFlightRegistry tracks consolidation windows the local pass-gate has
// claimed but not yet emitted a terminal (success/fail) event for. The
// watchdog consults it to skip the local runner's own active claims, so
// a sweep that fires while the runner is mid-pass cannot race ahead of
// the pending Success event and emit a spurious watchdog_expired fail.
//
// The registry is process-local and intentionally not persisted. A
// crash mid-pass leaves the claim with no Success/Fail in the event
// log; the next process boot starts with an empty registry, and the
// watchdog correctly observes the orphan and closes it out (the very
// situation the watchdog exists for). Persisting would defeat that.
//
// Safe for concurrent use; the pass-gate's Begin and the watchdog's
// IsActive routinely race in production.
type InFlightRegistry struct {
	mu      sync.Mutex
	windows map[string]struct{}
}

// NewInFlightRegistry returns a ready-to-use registry. The zero value
// is also usable for tests that want to assert the watchdog tolerates a
// nil registry (the production wiring always supplies one).
func NewInFlightRegistry() *InFlightRegistry {
	return &InFlightRegistry{windows: make(map[string]struct{})}
}

// Begin records that the local runner is about to invoke the inner
// PassFn for windowID. Pair with End in a defer so a panic in the
// inner function doesn't leave a stuck entry behind.
func (r *InFlightRegistry) Begin(windowID string) {
	if r == nil || windowID == "" {
		return
	}
	r.mu.Lock()
	if r.windows == nil {
		r.windows = make(map[string]struct{})
	}
	r.windows[windowID] = struct{}{}
	r.mu.Unlock()
}

// End clears the in-flight marker for windowID. Idempotent: calling
// End on an unknown window is a no-op so defer chains stay simple.
func (r *InFlightRegistry) End(windowID string) {
	if r == nil || windowID == "" {
		return
	}
	r.mu.Lock()
	delete(r.windows, windowID)
	r.mu.Unlock()
}

// IsActive reports whether the local runner is currently executing
// windowID. The watchdog checks this before emitting a fail; a true
// answer means "trust the runner, the Success event is on its way."
func (r *InFlightRegistry) IsActive(windowID string) bool {
	if r == nil || windowID == "" {
		return false
	}
	r.mu.Lock()
	_, ok := r.windows[windowID]
	r.mu.Unlock()
	return ok
}

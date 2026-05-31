package consolidation

import (
	"context"
	"time"
)

// SetQuietWaitHook overrides the quiet-period waiter so tests can drive
// the gap between the initial Decide and the post-quiet recheck
// deterministically — mutating rank state synchronously inside the hook
// instead of racing a real-time sleep from a separate goroutine. The hook
// receives the context and configured duration; returning a non-nil error
// is treated by the pass gate as a context cancellation.
func (e *Election) SetQuietWaitHook(fn func(ctx context.Context, d time.Duration) error) {
	e.quietWait = fn
}

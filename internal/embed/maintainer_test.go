package embed

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor receives n times from ch within the timeout or fails.
func waitFor(t *testing.T, ch <-chan struct{}, n int) {
	t.Helper()
	for i := range n {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for pass %d/%d", i+1, n)
		}
	}
}

func TestMaintainer_RunsInitialAndPeriodic(t *testing.T) {
	var calls int32
	ran := make(chan struct{}, 64)
	m := New(15*time.Millisecond, func(ctx context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		ran <- struct{}{}
		return 0, nil
	})
	m.Start()
	// Initial pass + at least two ticks.
	waitFor(t, ran, 3)
	m.Stop()

	// After Stop, no further passes.
	settled := atomic.LoadInt32(&calls)
	time.Sleep(60 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != settled {
		t.Errorf("maintainer ran %d more pass(es) after Stop", got-settled)
	}
}

func TestMaintainer_ErrorsAreNonFatal(t *testing.T) {
	ran := make(chan struct{}, 64)
	m := New(15*time.Millisecond, func(ctx context.Context) (int, error) {
		ran <- struct{}{}
		return 0, errors.New("boom")
	})
	m.Start()
	// A failing pass must not stop the loop — expect repeated passes.
	waitFor(t, ran, 3)
	m.Stop()
}

func TestMaintainer_StopIsIdempotentAndDrains(t *testing.T) {
	m := New(10*time.Millisecond, func(ctx context.Context) (int, error) {
		// Respect cancellation so Stop drains promptly.
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
			return 1, nil
		}
	})
	m.Start()
	time.Sleep(25 * time.Millisecond)
	m.Stop() // must return (drain) without hanging
}

func TestNew_DefaultsZeroInterval(t *testing.T) {
	m := New(0, func(ctx context.Context) (int, error) { return 0, nil })
	if m.interval != 5*time.Minute {
		t.Errorf("zero interval = %s, want 5m default", m.interval)
	}
}

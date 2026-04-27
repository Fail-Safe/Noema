package lock_test

import (
	"path/filepath"
	"testing"

	"github.com/Fail-Safe/Noema/internal/lock"
)

func TestTryAcquire_FreshPath_Succeeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "background.lock")

	l, got, err := lock.TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if !got {
		t.Errorf("got = false, want true (fresh path should acquire)")
	}
	if l == nil {
		t.Fatalf("lock = nil, want non-nil on success")
	}
	if err := l.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
}

func TestTryAcquire_HeldByAnother_ReturnsFalse(t *testing.T) {
	// Two TryAcquire calls in the same process from different fds
	// (different os.OpenFile invocations) create independent OFDs;
	// flock semantics make the second one EWOULDBLOCK while the
	// first holds the lock. This is the same behaviour we get when
	// the second caller is a different process — verifying it here
	// without spawning a subprocess.
	dir := t.TempDir()
	path := filepath.Join(dir, "background.lock")

	l1, got1, err := lock.TryAcquire(path)
	if err != nil {
		t.Fatalf("first TryAcquire: %v", err)
	}
	if !got1 {
		t.Fatalf("first acquire = false, want true")
	}
	defer l1.Release()

	l2, got2, err := lock.TryAcquire(path)
	if err != nil {
		t.Errorf("second TryAcquire returned error %v, want nil (contention is not an error)", err)
	}
	if got2 {
		t.Errorf("second acquire = true, want false (lock already held)")
	}
	if l2 != nil {
		t.Errorf("second lock = %v, want nil on contention", l2)
		l2.Release()
	}
}

func TestTryAcquire_AfterRelease_Reacquirable(t *testing.T) {
	// Releasing the first lock must allow a fresh acquire to
	// succeed. This is the "first process exits, next process
	// picks up the background work" handoff path.
	dir := t.TempDir()
	path := filepath.Join(dir, "background.lock")

	l1, got1, err := lock.TryAcquire(path)
	if err != nil || !got1 {
		t.Fatalf("first acquire failed: got=%v err=%v", got1, err)
	}
	if err := l1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	l2, got2, err := lock.TryAcquire(path)
	if err != nil {
		t.Fatalf("second TryAcquire: %v", err)
	}
	if !got2 {
		t.Errorf("re-acquire after release = false, want true")
	}
	if l2 != nil {
		l2.Release()
	}
}

func TestRelease_Idempotent(t *testing.T) {
	// Calling Release twice on the same lock must be a no-op the
	// second time, not an error and not a double-close. cmd_serve's
	// shutdown path may legitimately call Release during a deferred
	// cleanup even after some explicit earlier Release elsewhere.
	dir := t.TempDir()
	path := filepath.Join(dir, "background.lock")

	l, _, err := lock.TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("second Release returned %v, want nil (idempotent)", err)
	}
}

func TestRelease_NilLockSafe(t *testing.T) {
	// Defensive: deferred Release on a nil lock (the contention case
	// where TryAcquire returned nil) must not panic. Lets callers
	// write `defer l.Release()` without an `if l != nil` guard.
	var l *lock.Lock
	if err := l.Release(); err != nil {
		t.Errorf("nil-receiver Release returned %v, want nil", err)
	}
}

func TestTryAcquire_MissingDirectory_Errors(t *testing.T) {
	// Hard error when the lock path's parent doesn't exist — we want
	// callers to surface this as a startup failure rather than
	// silently fall through to MCP-only mode (which would mask a
	// misconfigured or corrupted cortex layout).
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent-subdir", "background.lock")

	l, got, err := lock.TryAcquire(path)
	if err == nil {
		t.Errorf("error = nil, want non-nil for missing parent directory")
	}
	if got {
		t.Errorf("got = true, want false on error path")
	}
	if l != nil {
		t.Errorf("lock = %v, want nil on error path", l)
		l.Release()
	}
}

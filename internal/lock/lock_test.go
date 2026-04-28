package lock_test

import (
	"os"
	"path/filepath"
	"strings"
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

// ---- RuntimePath ----

func TestRuntimePath_ReturnsValidPath(t *testing.T) {
	// Point the runtime base at a controlled temp dir for the test —
	// XDG_RUNTIME_DIR takes precedence over os.TempDir() so this is
	// a portable way to redirect the resolution. We restore the
	// environment afterward.
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	const cid = "01TESTCORTEXIDXXXXXXXXXXXX"
	path, err := lock.RuntimePath(cid)
	if err != nil {
		t.Fatalf("RuntimePath: %v", err)
	}
	if !strings.HasPrefix(path, dir) {
		t.Errorf("path %q does not start with runtime root %q", path, dir)
	}
	if !strings.Contains(path, cid) {
		t.Errorf("path %q does not contain cortex ID %q", path, cid)
	}
	if filepath.Base(path) != "background.lock" {
		t.Errorf("path %q does not end in background.lock", path)
	}

	// Parent directory must exist after the call — TryAcquire opens
	// the file with O_CREATE but does not mkdir its parents, so
	// RuntimePath must create them first.
	if _, statErr := os.Stat(filepath.Dir(path)); statErr != nil {
		t.Errorf("parent dir not created: %v", statErr)
	}
}

func TestRuntimePath_EmptyCortexIDErrors(t *testing.T) {
	// Empty cortex ID is a programmer error — we'd rather fail
	// loudly than create a "noema/" directory at the runtime root
	// shared across all cortexes (which would be a real footgun on
	// multi-cortex hosts).
	_, err := lock.RuntimePath("")
	if err == nil {
		t.Errorf("error = nil, want non-nil for empty cortex ID")
	}
}

func TestRuntimePath_StableAcrossCalls(t *testing.T) {
	// Two calls with the same cortex ID must produce the same path,
	// otherwise the lock-acquire / lock-release lifecycle wouldn't
	// reuse the same file across restarts.
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	a, err := lock.RuntimePath("01STABLECORTEXIDXXXXXXXXX1")
	if err != nil {
		t.Fatalf("first RuntimePath: %v", err)
	}
	b, err := lock.RuntimePath("01STABLECORTEXIDXXXXXXXXX1")
	if err != nil {
		t.Fatalf("second RuntimePath: %v", err)
	}
	if a != b {
		t.Errorf("paths differ across calls: %q vs %q", a, b)
	}
}

func TestRuntimePath_DistinctIDsCollide(t *testing.T) {
	// Different cortex IDs must produce different paths so two
	// cortexes on the same host can each hold their own lock
	// without any cross-cortex interference.
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	a, err := lock.RuntimePath("01CORTEXAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatalf("first RuntimePath: %v", err)
	}
	b, err := lock.RuntimePath("01CORTEXBBBBBBBBBBBBBBBBBB")
	if err != nil {
		t.Fatalf("second RuntimePath: %v", err)
	}
	if a == b {
		t.Errorf("distinct IDs collided to the same path: %q", a)
	}
}

func TestRuntimePath_HonoursXDGRuntimeDir(t *testing.T) {
	// XDG_RUNTIME_DIR is the Linux convention for tmpfs-backed
	// per-user runtime state. When set, we prefer it over
	// os.TempDir() so on systemd hosts the lock lives under
	// /run/user/$UID/noema/<cortex-id>/ rather than /tmp.
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	path, err := lock.RuntimePath("01XDGCORTEXIDXXXXXXXXXXXX1")
	if err != nil {
		t.Fatalf("RuntimePath: %v", err)
	}
	if !strings.HasPrefix(path, dir) {
		t.Errorf("XDG_RUNTIME_DIR not honoured: path %q is not under %q", path, dir)
	}
}

func TestRuntimePath_FallsBackToTempDir(t *testing.T) {
	// With XDG_RUNTIME_DIR explicitly cleared, the fallback path
	// must come from os.TempDir(). On the test host this is
	// /var/folders/.../T (macOS), /tmp (Linux), or %TEMP% (Windows)
	// — anywhere except an iCloud/Dropbox/etc. sync root.
	t.Setenv("XDG_RUNTIME_DIR", "")
	tmp := os.TempDir()

	path, err := lock.RuntimePath("01FALLBACKCORTEXIDXXXXXXXX")
	if err != nil {
		t.Fatalf("RuntimePath: %v", err)
	}
	if !strings.HasPrefix(path, tmp) {
		t.Errorf("fallback to os.TempDir() not honoured: path %q is not under %q", path, tmp)
	}
}

func TestRuntimePath_AcquirableViaTryAcquire(t *testing.T) {
	// End-to-end: RuntimePath returns a path that TryAcquire can
	// actually open and flock. This catches mode / permission /
	// parent-dir bugs that the unit tests above would miss.
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	path, err := lock.RuntimePath("01ACQUIRECORTEXIDXXXXXXXXX")
	if err != nil {
		t.Fatalf("RuntimePath: %v", err)
	}
	l, got, err := lock.TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if !got || l == nil {
		t.Fatalf("expected to acquire fresh runtime lock, got got=%v lock=%v", got, l)
	}
	if err := l.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
}

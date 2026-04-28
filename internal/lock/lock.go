// Package lock provides a thin wrapper around OS-level advisory file
// locking for coordinating which noema process on a given cortex
// directory runs background work (consolidator agent, eligibility
// loop, watchdog, filesystem watcher).
//
// The whole-process problem this solves: noema serve can be invoked
// concurrently against the same cortex via different transports — a
// long-lived `--transport http` systemd service plus any number of
// short-lived `--transport stdio` subprocesses spawned by MCP clients
// (Claude Code, Hermes plugins, etc.). All of them open the same
// SQLite DB in WAL mode, all of them load the same cortex_id, and all
// of them previously started their own consolidator agent +
// eligibility loop + watchdog + watcher. The result was duplicate
// event emissions, rank churn in federation_state, and
// consolidation_claim/fail noise from short-lived sessions whose
// schedulers fired briefly then got killed.
//
// OS-level advisory locks are the right primitive: locks are bound to
// the open file (descriptor on Unix, handle on Windows), the kernel
// releases them automatically on process exit (any path — clean,
// SIGTERM, SIGKILL, panic), and there's no stale-lock-file problem to
// clean up. We use exclusive non-blocking semantics so the loser
// observes contention immediately and falls back to MCP-only mode
// rather than blocking startup.
//
// Cross-platform implementation lives in lock_unix.go (flock(2)) and
// lock_windows.go (LockFileEx). The public API in this file is the
// same on both.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Lock represents an acquired exclusive file lock. The kernel releases
// the underlying lock when the file descriptor / handle is closed (or
// when the process exits), so a process that crashes without calling
// Release does not strand the lock for future invocations — that
// property is load-bearing and is the whole reason an OS-level lock
// was chosen over a PID-file or sentinel-presence scheme.
type Lock struct {
	mu       sync.Mutex
	f        *os.File
	released bool
}

// TryAcquire attempts to acquire an exclusive non-blocking lock on
// path. The file is created if it doesn't exist. Three return shapes:
//
//   - (lock, true, nil): we hold the lock; caller is responsible for
//     calling Release at shutdown (the kernel also releases on exit).
//   - (nil, false, nil): another process holds the lock; caller should
//     proceed without acquiring resources gated on lock ownership.
//   - (nil, false, err): something went wrong before we could ask the
//     kernel for a lock (e.g., parent directory missing, permission
//     denied). Treat as a hard error — don't silently fall through to
//     "no lock" because that masks misconfiguration.
//
// path should be inside the cortex dir's `db/` subdirectory so it
// doesn't show up in user-managed trace listings or get accidentally
// synced by tools like Obsidian or iCloud.
func TryAcquire(path string) (*Lock, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("open lock file %q: %w", path, err)
	}

	got, err := tryLock(f)
	if err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("lock %q: %w", path, err)
	}
	if !got {
		_ = f.Close()
		return nil, false, nil
	}
	return &Lock{f: f}, true, nil
}

// Release explicitly drops the lock and closes the file descriptor /
// handle. Safe to call multiple times; the second and subsequent
// calls are no-ops. Always pair every successful TryAcquire with a
// deferred Release for clarity, even though the kernel would
// auto-release on process exit. Safe to call on a nil receiver, so
// callers can `defer l.Release()` without an `if l != nil` guard.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true

	// Drop the kernel-side lock first, then close the fd. Closing
	// alone would also release the lock, but explicit unlock keeps
	// the intent obvious to readers and gives us a place to surface
	// platform-specific errors.
	unlockErr := unlock(l.f)
	closeErr := l.f.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlock: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close lock file: %w", closeErr)
	}
	return nil
}

// tryLock and unlock are platform-specific. See lock_unix.go and
// lock_windows.go for implementations. Both return:
//
//   - tryLock(f) (got bool, err error):
//       got=true:  acquired the lock; caller owns it
//       got=false, err=nil: contention (held by another process)
//       got=false, err!=nil: hard error (unrelated to contention)
//
//   - unlock(f) error: drops the lock; nil on success

// RuntimePath returns the per-cortex background-work lock path in a
// runtime/temp location that user-data sync layers (iCloud Drive,
// Dropbox, Syncthing, OneDrive) won't replicate. Keyed on the cortex's
// stable ULID so two cortexes with different IDs but the same display
// name don't collide on shared hosts.
//
// Why not <cortex>/db/background.lock (the previous location): when
// the cortex directory is inside a sync layer, the lock file gets
// replicated to other devices where it has no semantic meaning, and
// the sync layer's "replace on sync" can unlink the inode our flock
// is bound to and create a fresh inode at the same path — leaving us
// holding a flock on an orphaned inode while a new noema process
// successfully acquires its own flock on the new file. Putting the
// lock outside the cortex dir removes both problems uniformly,
// regardless of which sync layer the user has configured.
//
// Cross-host coordination is intentionally given up: kernel flocks
// are per-host and never propagated across machines anyway, so two
// hosts mounting the same cortex via a sync layer were always going
// to each acquire their own local flock. Federation HTTP is the
// designed cross-host coordination mechanism, not flock.
//
// Path resolution prefers $XDG_RUNTIME_DIR (Linux convention,
// tmpfs-backed when set), else os.TempDir() (which resolves
// correctly on macOS and Windows: per-user temp under
// /var/folders on macOS, %TEMP% on Windows).
//
// Creates the parent directory with 0700 permissions if missing.
func RuntimePath(cortexID string) (string, error) {
	if cortexID == "" {
		return "", errors.New("cortex ID required for runtime lock path")
	}
	base := filepath.Join(runtimeBase(), "noema", cortexID)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("creating runtime dir %q: %w", base, err)
	}
	return filepath.Join(base, "background.lock"), nil
}

// runtimeBase resolves the platform-appropriate runtime root. On
// Linux distributions that set $XDG_RUNTIME_DIR (typical on systemd
// systems — points at a tmpfs under /run/user/$UID), we use that.
// Otherwise os.TempDir() picks the right per-user temp on every
// supported platform: confstr(_CS_DARWIN_USER_TEMP_DIR) on macOS,
// $TMPDIR or /tmp on Linux without XDG_RUNTIME_DIR, %TEMP% on
// Windows. None of these locations are inside iCloud Drive or
// equivalent sync roots by default.
func runtimeBase() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return xdg
	}
	return os.TempDir()
}

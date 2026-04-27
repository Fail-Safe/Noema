// Package lock provides a thin wrapper around POSIX flock(2) for
// coordinating which noema process on a given cortex directory runs
// background work (consolidator agent, eligibility loop, watchdog,
// filesystem watcher).
//
// The whole-process problem this solves: noema serve can be invoked
// concurrently against the same cortex via different transports — a
// long-lived `--transport http` systemd service plus any number of
// short-lived `--transport stdio` subprocesses spawned by MCP clients
// (Claude Code, Hermes plugins, etc.). All of them open the same
// SQLite DB in WAL mode, all of them load the same cortex_id, and all
// of them currently start their own consolidator agent + eligibility
// loop + watchdog + watcher. The result is duplicate event emissions,
// rank churn in federation_state, and consolidation_claim/fail noise
// from short-lived sessions whose schedulers fire briefly then get
// killed.
//
// flock(2) is the right primitive: locks are associated with the open
// file description, the kernel releases them automatically on process
// exit (any path — clean, SIGTERM, SIGKILL, panic), and there's no
// stale-lock-file problem to clean up. We use LOCK_EX|LOCK_NB so the
// loser observes contention immediately and falls back to MCP-only
// mode rather than blocking startup.
//
// Unix-only by design — syscall.Flock isn't defined on Windows. The
// project's deployment targets are Linux (federated peers) and macOS
// (developer laptops); a Windows port would need a parallel
// LockFileEx implementation alongside this one.
package lock

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

// Lock represents an acquired exclusive flock on a sentinel file. The
// kernel releases the underlying lock when the file descriptor is
// closed (or when the process exits), so a process that crashes
// without calling Release does not strand the lock for future
// invocations — that property is load-bearing and is the whole reason
// flock was chosen over a PID-file or sentinel-presence scheme.
type Lock struct {
	mu       sync.Mutex
	f        *os.File
	released bool
}

// TryAcquire attempts to acquire an exclusive non-blocking flock on
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

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// EWOULDBLOCK / EAGAIN means the lock is held by someone
		// else — a normal, expected outcome and not a hard error.
		// Anything else (EBADF, ENOLCK, etc.) is unexpected.
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("flock %q: %w", path, err)
	}

	return &Lock{f: f}, true, nil
}

// Release explicitly drops the flock and closes the file descriptor.
// Safe to call multiple times; the second and subsequent calls are
// no-ops. Always pair every successful TryAcquire with a deferred
// Release for clarity, even though the kernel would auto-release on
// process exit.
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
	// alone would also release the lock, but explicit LOCK_UN keeps
	// the intent obvious to readers and gives us a place to surface
	// flock-specific errors.
	flockErr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	if flockErr != nil {
		return fmt.Errorf("flock unlock: %w", flockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close lock file: %w", closeErr)
	}
	return nil
}

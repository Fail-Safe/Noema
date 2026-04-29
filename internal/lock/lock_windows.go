//go:build windows

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// allBytes is the maximum lock-region size LockFileEx accepts: a
// 64-bit length split across two 32-bit fields. Combined with an
// offset of zero (in the Overlapped struct), this locks the entire
// file plus everything beyond EOF — Windows allows locking past the
// end, and using the full range is the standard idiom for whole-file
// advisory locks.
const allBytes = ^uint32(0)

// tryLock asks Windows for an exclusive immediate-fail lock on f.
// LOCKFILE_EXCLUSIVE_LOCK is the equivalent of flock's LOCK_EX;
// LOCKFILE_FAIL_IMMEDIATELY is the equivalent of LOCK_NB — without
// it the call would block until the lock is granted. Contention is
// surfaced as ERROR_LOCK_VIOLATION (the Win32 analogue of
// EWOULDBLOCK) and converted to (false, nil) so callers can
// distinguish "another process holds it" from real errors.
func tryLock(f *os.File) (bool, error) {
	overlapped := new(windows.Overlapped)
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,        // reserved, must be zero
		allBytes, // bytes-low
		allBytes, // bytes-high
		overlapped,
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

// unlock drops the LockFileEx lock. The same Overlapped struct shape
// (zeroed offsets) and byte range must match the lock call so
// Windows can identify which region to release. UnlockFileEx with a
// zero region offset and full-range length matches the lock we
// took above.
func unlock(f *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		allBytes,
		allBytes,
		overlapped,
	)
}

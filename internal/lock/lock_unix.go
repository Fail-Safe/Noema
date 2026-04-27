//go:build unix

package lock

import (
	"os"
	"syscall"
)

// tryLock asks the kernel for an exclusive non-blocking flock on f.
// EWOULDBLOCK / EAGAIN means another process holds the lock — a
// normal, expected outcome and not a hard error. Anything else
// (EBADF, ENOLCK, etc.) is unexpected and bubbles up.
func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if err == syscall.EWOULDBLOCK {
		return false, nil
	}
	return false, err
}

// unlock drops the flock. Closing the file would also release it,
// but explicit LOCK_UN keeps the lifecycle obvious.
func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

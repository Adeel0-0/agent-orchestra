//go:build windows

package bus

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const (
	guardRetryInterval = 10 * time.Millisecond
	guardTimeout       = 10 * time.Second
)

// lockGuardFile takes an exclusive lock on byte 0 (length 1) of f.
// Python's msvcrt.locking(fd, LK_NBLCK, 1) at offset 0 maps to LockFile on the
// same range, so a Go holder and a Python holder exclude each other.
// LK_LOCK would sleep a whole second between attempts, so both sides poll.
func lockGuardFile(f *os.File) (func() error, error) {
	handle := windows.Handle(f.Fd())
	deadline := time.Now().Add(guardTimeout)
	for {
		ol := new(windows.Overlapped) // Offset 0
		err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		if err == nil {
			return func() error {
				return windows.UnlockFileEx(handle, 0, 1, 0, new(windows.Overlapped))
			}, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, fmt.Errorf("locking %s: %w", f.Name(), err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for %s", f.Name())
		}
		time.Sleep(guardRetryInterval)
	}
}

// isSharingViolation reports whether err is the brief refusal Windows gives
// when another process has the file open while we rename or delete it.
func isSharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

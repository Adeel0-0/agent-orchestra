//go:build !windows

package bus

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockGuardFile takes a blocking exclusive flock on f, like Python's
// fcntl.flock(fd, LOCK_EX). The kernel drops it if the holder dies.
func lockGuardFile(f *os.File) (func() error, error) {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("locking %s: %w", f.Name(), err)
		}
		return func() error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }, nil
	}
}

func isSharingViolation(error) bool { return false }

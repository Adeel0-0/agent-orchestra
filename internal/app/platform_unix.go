//go:build !windows

package app

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockServerFile takes Python's fcntl.flock(fd, LOCK_EX | LOCK_NB) lock.
func lockServerFile(f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err != unix.EINTR {
			return err
		}
	}
}

// normcase is os.path.normcase on POSIX: unchanged.
func normcase(p string) string { return p }

// resolveStrict is Path(p).resolve(strict=True).
func resolveStrict(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

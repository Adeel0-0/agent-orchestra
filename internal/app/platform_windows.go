//go:build windows

package app

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// lockServerFile takes Python's msvcrt.locking(fd, LK_NBLCK, 1) lock: an exclusive,
// non-blocking lock on byte 0. A Python server and a Go server exclude each other.
func lockServerFile(f *os.File) error {
	ol := new(windows.Overlapped) // offset 0
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
}

// normcase is os.path.normcase on Windows: lower case and backslashes.
func normcase(p string) string {
	return strings.ToLower(strings.ReplaceAll(p, "/", `\`))
}

// resolveStrict is Path(p).resolve(strict=True): an absolute path to an existing
// item with links, junctions and subst drives resolved, as GetFinalPathNameByHandle
// reports it (the same call Python's realpath uses).
func resolveStrict(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	final, err := finalPathName(abs)
	if err == nil {
		return final, nil
	}
	if _, statErr := os.Stat(abs); statErr != nil {
		return "", statErr
	}
	// Some file systems refuse handle-based resolution; fall back to link evaluation.
	return filepath.EvalSymlinks(abs)
}

func finalPathName(p string) (string, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return stripExtendedPrefix(syscall.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n+1)
	}
}

// stripExtendedPrefix turns \\?\D:\x into D:\x and \\?\UNC\srv\share into \\srv\share.
func stripExtendedPrefix(p string) string {
	switch {
	case strings.HasPrefix(p, `\\?\UNC\`):
		return `\\` + p[len(`\\?\UNC\`):]
	case strings.HasPrefix(p, `\\?\`) && len(p) >= 6 && p[5] == ':':
		return p[4:]
	}
	return p
}

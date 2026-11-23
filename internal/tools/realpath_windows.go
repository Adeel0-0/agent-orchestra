//go:build windows

package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// maxLinkDepth bounds nested resolution of dangling links.
const maxLinkDepth = 32

// Errors that mean "this path cannot be opened as a whole; try its parent"
// (ntpath._getfinalpathname_nonstrict).
var walkUpErrors = map[syscall.Errno]bool{1: true, 2: true, 3: true, 5: true, 21: true, 32: true, 50: true,
	67: true, 87: true, 123: true, 161: true, 1920: true, 1921: true}

// Errors that end readlink-following without failing (ntpath._readlink_deep).
var readlinkStopErrors = map[syscall.Errno]bool{1: true, 2: true, 3: true, 5: true, 21: true, 32: true, 50: true,
	67: true, 87: true, 4390: true, 4392: true, 4393: true}

// realpath resolves symlinks and junctions like Python's ntpath.realpath
// (strict=False): the longest openable prefix is resolved by the OS
// (GetFinalPathNameByHandle) and the missing rest is appended. A dangling link
// is followed by reading it. Unlike Python, the followed target is resolved
// again, so a dangling link whose target passes through another link or
// junction cannot hide where a write would land.
func realpath(p string) (string, error) {
	return realpathDepth(p, 0)
}

func realpathDepth(p string, depth int) (string, error) {
	if depth > maxLinkDepth {
		return "", toolErr("Too many levels of links")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	path := filepath.Clean(abs)
	tail := ""
	for path != "" {
		final, err := finalPath(path)
		if err == nil {
			return joinTail(final, tail), nil
		}
		var errno syscall.Errno
		if !errors.As(err, &errno) || !walkUpErrors[errno] {
			return "", err
		}
		if target, err := readlinkDeep(path); err == nil && !strings.EqualFold(target, path) {
			return realpathDepth(joinTail(target, tail), depth+1)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return joinTail(path, tail), nil
		}
		tail = joinTail(filepath.Base(path), tail)
		path = parent
	}
	return tail, nil
}

func joinTail(base, tail string) string {
	if tail == "" {
		return base
	}
	return filepath.Join(base, tail)
}

// finalPath opens p (following links) and asks Windows for its normalized path.
func finalPath(p string) (string, error) {
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
			return stripExtendedPrefix(windows.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n+1)
	}
}

func stripExtendedPrefix(p string) string {
	switch {
	case strings.HasPrefix(p, `\\?\UNC\`):
		return `\\` + p[len(`\\?\UNC\`):]
	case strings.HasPrefix(p, `\\?\`):
		return p[len(`\\?\`):]
	}
	return p
}

// readlinkDeep follows a chain of links whose targets do not exist.
func readlinkDeep(p string) (string, error) {
	seen := map[string]bool{}
	for !seen[strings.ToLower(p)] {
		seen[strings.ToLower(p)] = true
		old := p
		target, err := os.Readlink(p)
		if err != nil {
			var errno syscall.Errno
			if errors.As(err, &errno) && readlinkStopErrors[errno] {
				break
			}
			return "", err
		}
		p = target
		if !filepath.IsAbs(p) {
			if sym, junction := linkKind(old); !sym && !junction {
				p = old
				break
			}
			p = filepath.Clean(filepath.Join(filepath.Dir(old), p))
		}
	}
	return p, nil
}

// reparseTag returns p's reparse tag, or 0 if p is not a reparse point.
func reparseTag(p string) uint32 {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0
	}
	var data windows.Win32finddata
	h, err := windows.FindFirstFile(name, &data)
	if err != nil {
		return 0
	}
	windows.FindClose(h)
	if data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return 0
	}
	return data.Reserved0
}

// linkKind reports whether p is a symlink or a junction (mount point).
func linkKind(p string) (symlink, junction bool) {
	switch reparseTag(p) {
	case windows.IO_REPARSE_TAG_SYMLINK:
		return true, false
	case windows.IO_REPARSE_TAG_MOUNT_POINT:
		return false, true
	}
	return false, false
}

// linkCount is the hard-link count of the file p refers to (following links).
func linkCount(p string) (uint64, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, fmt.Errorf("GetFileInformationByHandle: %w", err)
	}
	return uint64(info.NumberOfLinks), nil
}

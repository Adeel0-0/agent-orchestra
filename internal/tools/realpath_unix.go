//go:build !windows

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// realpath resolves symlinks like Python's posixpath.realpath(strict=False):
// components are visited left to right, so ".." applies to the resolved parent,
// and missing components are kept as written.
func realpath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		p = abs
	}
	out, _ := joinRealpath("", p, map[string]*string{})
	return filepath.Clean(out), nil
}

func joinRealpath(path, rest string, seen map[string]*string) (string, bool) {
	if strings.HasPrefix(rest, "/") {
		rest = rest[1:]
		path = "/"
	}
	for rest != "" {
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			if path != "" {
				dir, base := splitPosix(path)
				path = dir
				if base == ".." {
					path = joinPosix(path, "..", "..")
				}
			} else {
				path = ".."
			}
			continue
		}
		newpath := joinPosix(path, name)
		info, err := os.Lstat(newpath)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			path = newpath
			continue
		}
		if resolved, ok := seen[newpath]; ok {
			if resolved != nil {
				path = *resolved
				continue
			}
			return joinPosix(newpath, rest), false // symlink loop
		}
		seen[newpath] = nil
		target, err := os.Readlink(newpath)
		if err != nil {
			path = newpath
			continue
		}
		var ok bool
		path, ok = joinRealpath(path, target, seen)
		if !ok {
			return joinPosix(path, rest), false
		}
		resolved := path
		seen[newpath] = &resolved
	}
	return path, true
}

// splitPosix is posixpath.split: (head without trailing slashes, tail).
func splitPosix(p string) (string, string) {
	i := strings.LastIndex(p, "/") + 1
	head, tail := p[:i], p[i:]
	if head != "" && strings.Trim(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head, tail
}

// joinPosix is posixpath.join.
func joinPosix(a string, parts ...string) string {
	out := a
	for _, b := range parts {
		switch {
		case strings.HasPrefix(b, "/"):
			out = b
		case out == "" || strings.HasSuffix(out, "/"):
			out += b
		default:
			out += "/" + b
		}
	}
	return out
}

// linkKind reports whether p itself is a symlink. POSIX has no junctions.
func linkKind(p string) (symlink, junction bool) {
	info, err := os.Lstat(p)
	return err == nil && info.Mode()&os.ModeSymlink != 0, false
}

// linkCount is the hard-link count of the file p refers to (following links).
func linkCount(p string) (uint64, error) {
	info, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink), nil
	}
	return 1, nil
}

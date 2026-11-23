package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const isWindows = runtime.GOOS == "windows"

// isSep reports a path separator: '/' everywhere, and '\' on Windows.
func isSep(c byte) bool { return c == '/' || (isWindows && c == '\\') }

// lexicalParts splits p into its components after the volume and root, like
// pathlib's parts without the anchor: empty and "." components are dropped and
// ".." is kept (pathlib does not collapse it).
func lexicalParts(p string) []string {
	p = p[len(filepath.VolumeName(p)):]
	var parts []string
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || isSep(p[i]) {
			if part := p[start:i]; part != "" && part != "." {
				parts = append(parts, part)
			}
			start = i + 1
		}
	}
	return parts
}

// joinLexical joins path onto root the way pathlib's `root / path` does: an
// absolute path replaces root, a rooted path without a drive keeps root's drive,
// and a drive-relative path on another drive is made absolute on that drive.
// Nothing is cleaned, so ".." stays visible to the metadata check.
func joinLexical(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if !isWindows {
		return root + string(os.PathSeparator) + path
	}
	vol := filepath.VolumeName(path)
	rootVol := filepath.VolumeName(root)
	switch {
	case vol != "" && strings.EqualFold(vol, rootVol):
		return root + `\` + path[len(vol):]
	case vol != "":
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
		return path
	case len(path) > 0 && isSep(path[0]):
		return rootVol + path
	}
	return root + `\` + path
}

// isWithin reports whether target is root or below it. Both must be absolute and
// clean. Windows compares case-insensitively, like pathlib.
func isWithin(root, target string) bool {
	if isWindows {
		if !strings.EqualFold(filepath.VolumeName(root), filepath.VolumeName(target)) {
			return false
		}
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// relativeTo returns target relative to root (target must be within root).
func relativeTo(root, target string) string {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return target
	}
	return rel
}

// toPosix shows a relative path with forward slashes, like as_posix().
func toPosix(rel string) string {
	if rel == "." {
		return "."
	}
	return filepath.ToSlash(rel)
}

// reservedNames are Windows device names (Python 3.12 PureWindowsPath.is_reserved).
var reservedNames = func() map[string]bool {
	m := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}
	for _, d := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
		m["COM"+d] = true
		m["LPT"+d] = true
	}
	return m
}()

// isReservedPart applies is_reserved to one component: the name before the first
// '.' and ':' with trailing spaces removed, case-insensitively.
func isReservedPart(part string) bool {
	name, _, _ := strings.Cut(part, ".")
	name, _, _ = strings.Cut(name, ":")
	name = strings.TrimRight(name, " ")
	return reservedNames[strings.ToUpper(name)]
}

// isProtectedPart matches .git and .orchestra, ignoring case and the trailing
// dots and spaces Windows strips from names.
func isProtectedPart(part string) bool {
	name := strings.ToLower(strings.TrimRight(part, " ."))
	return name == ".git" || name == ".orchestra"
}

// isDevicePath matches Win32 device and extended-length prefixes.
func isDevicePath(p string) bool {
	if len(p) < 3 {
		return false
	}
	if isSep(p[0]) && isSep(p[1]) && (p[2] == '?' || p[2] == '.') {
		return true
	}
	return strings.HasPrefix(p, `\??\`)
}

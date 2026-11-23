package tools

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// FileTools are the bounded text tools, confined to Root (orchestra/tools/fs.py).
type FileTools struct {
	Root      string // resolved project directory
	ScanLimit int
}

// excludedDirs are never searched.
var excludedDirs = map[string]bool{".git": true, ".orchestra": true, "node_modules": true, ".venv": true, "__pycache__": true}

// fileToolNames are the tools Execute runs.
var fileToolNames = map[string]bool{"read_file": true, "list_dir": true, "glob": true, "grep": true,
	"write_file": true, "edit_file": true}

// NewFileTools resolves root (which must be an existing directory). A
// scanLimit <= 0 means DefaultScanLimit.
func NewFileTools(root string, scanLimit int) (*FileTools, error) {
	resolved, err := resolveStrict(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, toolErr("Project root must be a directory")
	}
	if scanLimit <= 0 {
		scanLimit = DefaultScanLimit
	}
	return &FileTools{Root: resolved, ScanLimit: scanLimit}, nil
}

// resolveStrict resolves an existing path, failing if any part is missing.
func resolveStrict(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	return realpath(abs)
}

// Resolve confines a model-supplied path to the project and returns the resolved
// absolute path. write additionally refuses .git/.orchestra metadata (lexical and
// resolved, so junction and symlink aliases count) and hardlinked files.
// Concurrent filesystem changes by another process are not guarded against.
func (t *FileTools) Resolve(path string, write bool) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", toolErr("path must be a non-empty string")
	}
	if isWindows && isDevicePath(path) {
		return "", toolErr("Device paths and alternate streams are not supported")
	}
	joined := joinLexical(t.Root, path)
	// Refuse lexical escapes before touching the filesystem: an absolute or UNC
	// path elsewhere is never opened (no network access for \\host paths).
	if !isWithin(t.Root, filepath.Clean(joined)) {
		return "", toolErr("Path leaves the project root")
	}
	if isWindows {
		for _, part := range lexicalParts(filepath.Clean(joined)) {
			if isReservedPart(part) {
				return "", toolErr("Reserved Windows path")
			}
		}
	}
	target, err := realpath(joined)
	if err != nil {
		return "", err
	}
	if !isWithin(t.Root, target) {
		return "", toolErr("Path leaves the project root")
	}
	if isWindows {
		if strings.Contains(relativeTo(t.Root, target), ":") {
			return "", toolErr("Device paths and alternate streams are not supported")
		}
		for _, part := range lexicalParts(target) {
			if isReservedPart(part) {
				return "", toolErr("Reserved Windows path")
			}
		}
	}
	if write {
		for _, candidate := range []string{joined, target} {
			for _, part := range lexicalParts(candidate) {
				if isProtectedPart(part) {
					return "", toolErr("Writes to .git and .orchestra metadata are forbidden")
				}
			}
		}
		if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
			if n, err := linkCount(target); err == nil && n > 1 {
				return "", toolErr("Writes to hardlinked files are forbidden; the target may alias protected metadata")
			}
		}
	}
	return target, nil
}

// Read returns a UTF-8 text file of at most FileLimit bytes.
func (t *FileTools) Read(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, FileLimit+1))
	if err != nil {
		return "", err
	}
	if len(raw) > FileLimit {
		return "", toolErr("File exceeds 1 MiB text-tool limit")
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return "", toolErr("Binary files are not supported")
	}
	if !utf8.Valid(raw) {
		return "", toolErr("'utf-8' codec can't decode the file: invalid UTF-8")
	}
	return string(raw), nil
}

var errScanLimit = errors.New("scan limit reached")

// walkFiles visits files below start like os.walk(followlinks=False): each
// directory's files (sorted) before its subdirectories (sorted). Links,
// junctions, generated directories and anything resolving outside the project
// are skipped. Unreadable directories are skipped silently.
func (t *FileTools) walkFiles(start string, visit func(path string) bool) error {
	scanned := 0
	var walk func(dir string) (bool, error)
	walk = func(dir string) (bool, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return true, nil
		}
		var dirs, files []string
		for _, e := range entries {
			if entryIsDir(filepath.Join(dir, e.Name()), e) {
				dirs = append(dirs, e.Name())
			} else {
				files = append(files, e.Name())
			}
		}
		sort.Strings(files)
		sort.Strings(dirs)
		for _, name := range files {
			scanned++
			if scanned > t.ScanLimit {
				return false, errScanLimit
			}
			p := filepath.Join(dir, name)
			if sym, _ := linkKind(p); sym {
				continue
			}
			if resolved, err := realpath(p); err != nil || !isWithin(t.Root, resolved) {
				continue
			}
			if !visit(p) {
				return false, nil
			}
		}
		for _, name := range dirs {
			if excludedDirs[name] {
				continue
			}
			p := filepath.Join(dir, name)
			if sym, junction := linkKind(p); sym || junction {
				continue
			}
			if resolved, err := realpath(p); err != nil || !isWithin(t.Root, resolved) {
				continue
			}
			if more, err := walk(p); !more || err != nil {
				return more, err
			}
		}
		return true, nil
	}
	_, err := walk(start)
	return err
}

// entryIsDir is DirEntry.is_dir() with links followed (as os.walk sorts entries).
func entryIsDir(p string, e os.DirEntry) bool {
	if e.Type()&os.ModeSymlink == 0 && e.Type()&os.ModeIrregular == 0 {
		return e.IsDir()
	}
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// Execute runs one file tool. Errors are tool failures for the model.
func (t *FileTools) Execute(name string, args map[string]any) (Result, error) {
	if !fileToolNames[name] {
		return Result{}, toolErr("Unknown tool: %s", name)
	}
	dot := "."
	raw, err := StringArg(args, "path", &dot)
	if err != nil {
		return Result{}, err
	}
	path, err := t.Resolve(raw, name == "write_file" || name == "edit_file")
	if err != nil {
		return Result{}, err
	}
	switch name {
	case "read_file":
		return t.readFile(path, args)
	case "list_dir":
		return t.listDir(path)
	case "glob", "grep":
		return t.search(name, path, args)
	}
	return t.write(name, path, args)
}

func (t *FileTools) readFile(path string, args map[string]any) (Result, error) {
	text, err := t.Read(path)
	if err != nil {
		return Result{}, err
	}
	offset, limit := 0, 200
	rawOffset, hasOffset := args["offset"]
	rawLimit, hasLimit := args["limit"]
	okOffset, okLimit := true, true
	if hasOffset {
		offset, okOffset = intArg(rawOffset)
	}
	if hasLimit {
		limit, okLimit = intArg(rawLimit)
	}
	if !okOffset || offset < 0 || !okLimit || limit < 1 || limit > 2000 {
		return Result{}, toolErr("offset must be a non-negative line index; limit must be 1 to 2000 lines")
	}
	lines := splitLines(text, true)
	end := min(offset, len(lines))
	var selected []string
	size := 0
	if offset < len(lines) {
		for _, line := range lines[offset:min(len(lines), offset+limit)] {
			n := utf8.RuneCountInString(line)
			if size+n > OutputLimit-200 {
				if len(selected) == 0 {
					return Result{OK: true, Output: Bounded(line, OutputLimit-200) +
						fmt.Sprintf("\n[line %d exceeds output cap; use an approved command to inspect the long line]", offset+1)}, nil
				}
				break
			}
			selected = append(selected, line)
			size += n
			end++
		}
	}
	if !hasOffset && !hasLimit && end == len(lines) {
		return Result{OK: true, Output: text}, nil
	}
	first := end
	if len(selected) > 0 {
		first = offset + 1
	}
	footer := fmt.Sprintf("\n[lines %d-%d of %d; %d lines remain; next offset: %d]",
		first, end, len(lines), len(lines)-end, end)
	return Result{OK: true, Output: strings.Join(selected, "") + footer}, nil
}

func (t *FileTools) listDir(path string) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return Result{}, err
	}
	var names []string
	for _, e := range entries {
		if len(names) >= 1000 {
			names = append(names, "[listing truncated]")
			break
		}
		name := e.Name()
		if dirNoFollow(filepath.Join(path, name), e) {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return Result{OK: true, Output: Bounded(strings.Join(names, "\n"), OutputLimit)}, nil
}

// dirNoFollow is DirEntry.is_dir(follow_symlinks=False): a junction counts as a
// directory, a directory symlink does not.
func dirNoFollow(p string, e os.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	if isWindows && e.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		if _, junction := linkKind(p); junction {
			return true
		}
	}
	return false
}

func (t *FileTools) search(name, path string, args map[string]any) (Result, error) {
	pattern, err := StringArg(args, "pattern", nil)
	if err != nil {
		return Result{}, err
	}
	patternParts := lexicalParts(pattern)
	if name == "glob" {
		if filepath.IsAbs(pattern) {
			return Result{}, toolErr("glob pattern must stay inside the project")
		}
		for _, part := range patternParts {
			if part == ".." {
				return Result{}, toolErr("glob pattern must stay inside the project")
			}
		}
		if vol := filepath.VolumeName(pattern); vol != "" {
			patternParts = append([]string{vol}, patternParts...)
		}
	}
	var lines []string
	size := 0
	done := false
	visit := func(p string) bool {
		rel := toPosix(relativeTo(t.Root, p))
		var matches []string
		if name == "glob" {
			if globMatches(lexicalParts(relativeTo(path, p)), patternParts) {
				matches = []string{rel}
			}
		} else {
			text, err := t.Read(p)
			if err != nil {
				return true
			}
			for i, line := range splitLines(text, false) {
				if strings.Contains(line, pattern) {
					matches = append(matches, fmt.Sprintf("%s:%d:%s", rel, i+1, line))
				}
			}
		}
		for _, m := range matches {
			lines = append(lines, m)
			size += utf8.RuneCountInString(m) + 1
			if size > OutputLimit {
				done = true
				return false
			}
		}
		return true
	}
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		visit(path)
	} else if err := t.walkFiles(path, visit); errors.Is(err, errScanLimit) && !done {
		lines = append(lines, fmt.Sprintf("[scan truncated after %d files; narrow the search path]", t.ScanLimit))
	}
	return Result{OK: true, Output: Bounded(strings.Join(lines, "\n"), OutputLimit)}, nil
}

// globMatches is an anchored path glob: '*' stays in one segment and '**' spans
// zero or more segments.
func globMatches(parts, patterns []string) bool {
	memo := map[[2]int]bool{}
	seen := map[[2]int]bool{}
	var match func(i, j int) bool
	match = func(i, j int) bool {
		key := [2]int{i, j}
		if seen[key] {
			return memo[key]
		}
		var ok bool
		switch {
		case j == len(patterns):
			ok = i == len(parts)
		case patterns[j] == "**":
			ok = match(i, j+1) || (i < len(parts) && match(i+1, j))
		default:
			ok = i < len(parts) && fnmatchCase(parts[i], patterns[j]) && match(i+1, j+1)
		}
		seen[key], memo[key] = true, ok
		return ok
	}
	return match(0, 0)
}

func (t *FileTools) write(name, path string, args map[string]any) (Result, error) {
	_, statErr := os.Stat(path)
	existed := statErr == nil
	var text string
	if name == "write_file" {
		content, err := StringArg(args, "content", nil)
		if err != nil {
			return Result{}, err
		}
		text = content
	} else {
		old, err := StringArg(args, "old", nil)
		if err != nil {
			return Result{}, err
		}
		replacement, err := StringArg(args, "new", nil)
		if err != nil {
			return Result{}, err
		}
		current, err := t.Read(path)
		if err != nil {
			return Result{}, err
		}
		if old == "" || strings.Count(current, old) != 1 {
			return Result{}, toolErr("edit_file requires exactly one non-empty match; file unchanged")
		}
		text = strings.Replace(current, old, replacement, 1)
	}
	if len(text) > FileLimit {
		return Result{}, toolErr("Write exceeds 1 MiB text-tool limit")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(path, []byte(text), 0o666); err != nil {
		return Result{}, err
	}
	kind := "add"
	if existed {
		kind = "update"
	}
	return Result{OK: true, Output: "File written.", Path: toPosix(relativeTo(t.Root, path)), Kind: kind}, nil
}

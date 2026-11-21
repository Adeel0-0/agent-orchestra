// Package watcher reports file changes under the watched paths, debounced, like
// orchestra/watcher.py. It uses fsnotify instead of polling; directories are
// added recursively, including ones created later.
package watcher

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher calls OnChange with absolute paths once activity has been quiet for Debounce.
type Watcher struct {
	Root     string
	Paths    []string
	Ignore   []string
	Debounce time.Duration
	OnChange func([]string)

	fs       *fsnotify.Watcher
	patterns []*regexp.Regexp
	stop     chan struct{}
	done     chan struct{}
}

// New prepares a watcher; call Start to begin.
func New(root string, watchPaths, ignore []string, debounce time.Duration, onChange func([]string)) *Watcher {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	w := &Watcher{Root: abs, Ignore: ignore, Debounce: debounce, OnChange: onChange}
	for _, p := range watchPaths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(abs, p)
		}
		w.Paths = append(w.Paths, filepath.Clean(p))
	}
	for _, pat := range ignore {
		w.patterns = append(w.patterns, Translate(normalize(pat)))
	}
	return w
}

func normalize(s string) string {
	s = strings.ReplaceAll(s, `\`, "/")
	if runtime.GOOS == "windows" {
		s = strings.ToLower(s) // fnmatch uses os.path.normcase, which is case-insensitive on Windows
	}
	return s
}

// ShouldIgnore applies the ignore patterns to a path relative to Root, with
// Python's rules: fnmatch, plus "dir/*" also ignoring anything starting with "dir".
func (w *Watcher) ShouldIgnore(rel string) bool {
	norm := normalize(rel)
	for i, pat := range w.Ignore {
		if w.patterns[i].MatchString(norm) {
			return true
		}
		p := normalize(pat)
		if strings.HasSuffix(p, "/*") && strings.HasPrefix(norm, strings.TrimSuffix(p, "/*")) {
			return true
		}
	}
	return false
}

func (w *Watcher) rel(path string) (string, bool) {
	rel, err := filepath.Rel(w.Root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// Start begins watching in the background.
func (w *Watcher) Start() error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.fs = fw
	for _, p := range w.Paths {
		w.addTree(p)
	}
	w.stop, w.done = make(chan struct{}), make(chan struct{})
	go w.loop()
	return nil
}

// Stop ends watching and waits for the loop to finish.
func (w *Watcher) Stop() {
	if w.fs == nil {
		return
	}
	close(w.stop)
	<-w.done
	w.fs.Close()
	w.fs = nil
}

// addTree watches dir and every non-ignored directory below it.
func (w *Watcher) addTree(root string) {
	info, err := os.Stat(root)
	if err != nil {
		return
	}
	if !info.IsDir() {
		w.fs.Add(filepath.Dir(root)) // a single watched file: watch its folder, filter in the loop
		return
	}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if rel, ok := w.rel(path); ok && rel != "." && w.ShouldIgnore(rel) {
			return filepath.SkipDir
		}
		w.fs.Add(path)
		return nil
	})
}

func (w *Watcher) watched(path string) bool {
	for _, p := range w.Paths {
		if path == p || strings.HasPrefix(path, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (w *Watcher) loop() {
	defer close(w.done)
	pending := map[string]bool{}
	var mu sync.Mutex
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		select {
		case <-w.stop:
			timer.Stop()
			return
		case err, ok := <-w.fs.Errors:
			if !ok {
				return
			}
			_ = err // like Python's polling loop, a watch error is not fatal
		case ev, ok := <-w.fs.Events:
			if !ok {
				return
			}
			if ev.Op == fsnotify.Chmod || !w.watched(ev.Name) {
				continue
			}
			rel, ok := w.rel(ev.Name)
			if !ok || w.ShouldIgnore(rel) {
				continue
			}
			if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
				if ev.Has(fsnotify.Create) {
					w.addTree(ev.Name)
				}
				continue // Python reports files only
			}
			mu.Lock()
			pending[ev.Name] = true
			mu.Unlock()
			timer.Reset(w.Debounce)
		case <-timer.C:
			mu.Lock()
			paths := make([]string, 0, len(pending))
			for p := range pending {
				paths = append(paths, p)
			}
			pending = map[string]bool{}
			mu.Unlock()
			if len(paths) > 0 && w.OnChange != nil {
				sort.Strings(paths)
				func() {
					defer func() { recover() }() // a failing callback must not stop the watcher
					w.OnChange(paths)
				}()
			}
		}
	}
}

// Translate converts an fnmatch pattern to an anchored regexp the way Python's
// fnmatch.translate does: '*' matches anything including '/', '?' one character,
// and [seq] / [!seq] character classes.
func Translate(pat string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^(?s:")
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			j := i + 1
			if j < len(pat) && pat[j] == '!' {
				j++
			}
			if j < len(pat) && pat[j] == ']' {
				j++
			}
			for j < len(pat) && pat[j] != ']' {
				j++
			}
			if j >= len(pat) {
				b.WriteString(`\[`)
				continue
			}
			class := pat[i+1 : j]
			class = strings.ReplaceAll(class, `\`, `\\`)
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			} else if strings.HasPrefix(class, "^") {
				class = `\` + class
			}
			b.WriteString("[" + class + "]")
			i = j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(")$")
	return regexp.MustCompile(b.String())
}

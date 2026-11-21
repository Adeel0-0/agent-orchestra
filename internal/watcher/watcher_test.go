package watcher

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var defaultIgnore = []string{".git/*", ".orchestra/*", "node_modules/*", "__pycache__/*", "*.pyc", "*.log", ".next/*", "dist/*", "build/*"}

func TestShouldIgnoreMatchesPythonRules(t *testing.T) {
	w := New(t.TempDir(), []string{"."}, defaultIgnore, time.Millisecond, nil)
	cases := map[string]bool{
		"src/app.py":                false,
		"src/app.pyc":               true, // fnmatch '*' crosses '/'
		filepath.Join("a", "b.log"): true,
		".git/HEAD":                 true,
		".orchestra/events.jsonl":   true,
		"node_modules/x/index.js":   true,
		"dist":                      true, // "dist/*" prefix rule
		".gitignore":                true, // Python quirk: ".git/*" also matches the ".git" prefix
		"docs/build.md":             false,
	}
	for rel, want := range cases {
		if got := w.ShouldIgnore(rel); got != want {
			t.Errorf("ShouldIgnore(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestTranslateCharacterClasses(t *testing.T) {
	if !Translate("file[0-9].txt").MatchString("file7.txt") || Translate("file[!0-9].txt").MatchString("file7.txt") {
		t.Fatal("character classes")
	}
	if !Translate("a?c").MatchString("abc") || Translate("a?c").MatchString("ac") {
		t.Fatal("? wildcard")
	}
}

func TestReportsChangedFilesAfterDebounceAndSkipsIgnored(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "src"), 0o755)
	var mu sync.Mutex
	var got []string
	w := New(root, []string{"."}, defaultIgnore, 200*time.Millisecond, func(paths []string) {
		mu.Lock()
		got = append(got, paths...)
		mu.Unlock()
	})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	os.WriteFile(filepath.Join(root, "src", "a.py"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "debug.log"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(root, "newdir"), 0o755)
	time.Sleep(300 * time.Millisecond) // let the new directory be added
	os.WriteFile(filepath.Join(root, "newdir", "b.py"), []byte("x"), 0o644)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		seen := map[string]bool{}
		for _, p := range got {
			seen[p] = true
		}
		mu.Unlock()
		if seen[filepath.Join(root, "src", "a.py")] && seen[filepath.Join(root, "newdir", "b.py")] {
			if seen[filepath.Join(root, "debug.log")] {
				t.Fatal("ignored file was reported")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("changes not reported, got %v", got)
}

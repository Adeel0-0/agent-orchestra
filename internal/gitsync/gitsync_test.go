package gitsync

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParsePorcelainZKeepsFirstCharacterAndHandlesRenames(t *testing.T) {
	out := " M src/app.py\x00?? new file.txt\x00R  renamed.go\x00old.go\x00A  added\x00"
	got := ParsePorcelainZ(out)
	want := []string{"src/app.py", "new file.txt", "renamed.go", "added"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := ParsePorcelainZ(""); len(got) != 0 {
		t.Fatalf("empty status gave %q", got)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestStatusAgainstARealRepository(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "t@example.com")
	git(t, repo, "config", "user.name", "t")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("1"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")

	e := New(repo)
	if !e.IsGitRepo() {
		t.Fatal("not detected as a repo")
	}
	st := e.Status()
	if st.Branch != "main" || !st.IsClean || st.HeadSHA == "unknown" {
		t.Fatalf("%+v", st)
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("2"), 0o644)
	st = e.Status()
	if st.IsClean || !reflect.DeepEqual(st.ChangedFiles, []string{"a.txt"}) {
		t.Fatalf("modified file: %+v", st)
	}
	if e.Push("nowhere", "main") || e.LastError == "" {
		t.Fatal("failed push must record its error")
	}
	if New(t.TempDir()).IsGitRepo() {
		t.Fatal("a plain folder is not a repo")
	}
}

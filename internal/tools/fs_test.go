package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newTools(t *testing.T) (*FileTools, string) {
	t.Helper()
	ft, err := NewFileTools(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return ft, ft.Root
}

func mustExec(t *testing.T, ft *FileTools, name string, args map[string]any) Result {
	t.Helper()
	r, err := ft.Execute(name, args)
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return r
}

func writeFile(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// jsonArgs decodes like the player does (json.Number for numbers).
func jsonArgs(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestWriteReadEditAndFind(t *testing.T) {
	ft, _ := newTools(t)
	mustExec(t, ft, "write_file", map[string]any{"path": "src/a.py", "content": "hello\nhello again\n"})
	mustExec(t, ft, "edit_file", map[string]any{"path": "src/a.py", "old": "hello again", "new": "goodbye"})
	if got := mustExec(t, ft, "read_file", map[string]any{"path": "src/a.py"}).Output; got != "hello\ngoodbye\n" {
		t.Fatalf("read = %q", got)
	}
	if got := mustExec(t, ft, "glob", map[string]any{"pattern": "**/*.py"}).Output; !strings.Contains(got, "src/a.py") {
		t.Fatalf("glob = %q", got)
	}
	if got := mustExec(t, ft, "grep", map[string]any{"pattern": "goodbye"}).Output; !strings.Contains(got, "src/a.py:2:goodbye") {
		t.Fatalf("grep = %q", got)
	}
	if got := mustExec(t, ft, "list_dir", map[string]any{}).Output; !strings.Contains(got, "src/") {
		t.Fatalf("list_dir = %q", got)
	}
}

func TestWriteReportsAddThenUpdate(t *testing.T) {
	ft, _ := newTools(t)
	r := mustExec(t, ft, "write_file", map[string]any{"path": "dir/x.txt", "content": "1"})
	if r.Path != "dir/x.txt" || r.Kind != "add" {
		t.Fatalf("first write = %+v", r)
	}
	r = mustExec(t, ft, "write_file", map[string]any{"path": "dir/x.txt", "content": "2"})
	if r.Kind != "update" {
		t.Fatalf("second write = %+v", r)
	}
}

func TestAmbiguousEditChangesNothing(t *testing.T) {
	ft, root := newTools(t)
	p := filepath.Join(root, "a.txt")
	writeFile(t, p, "same same")
	if _, err := ft.Execute("edit_file", map[string]any{"path": "a.txt", "old": "same", "new": "x"}); err == nil {
		t.Fatal("ambiguous edit succeeded")
	}
	if readText(t, p) != "same same" {
		t.Fatal("file changed")
	}
}

func TestGlobStarIsOneSegmentAndDoubleStarIncludesRoot(t *testing.T) {
	ft, root := newTools(t)
	writeFile(t, filepath.Join(root, "main.py"), "root")
	writeFile(t, filepath.Join(root, "src", "a.py"), "nested")
	if got := mustExec(t, ft, "glob", map[string]any{"pattern": "*.py"}).Output; got != "main.py" {
		t.Fatalf("*.py = %q", got)
	}
	if got := mustExec(t, ft, "glob", map[string]any{"pattern": "**/*.py"}).Output; got != "main.py\nsrc/a.py" {
		t.Fatalf("**/*.py = %q", got)
	}
}

func TestFnmatchSets(t *testing.T) {
	cases := []struct {
		name, pattern string
		want          bool
	}{
		{"a.py", "[abc].py", true}, {"d.py", "[!abc].py", true}, {"a.py", "[!abc].py", false},
		{"^.py", "[^x].py", true}, {"b.py", "[a-c].py", true}, {"b.py", "[c-a].py", false},
		{"[.py", "[.py", true}, {`a\b`, `a\b`, true}, {"x", "?", true}, {"xy", "?", false},
	}
	for _, c := range cases {
		if got := fnmatchCase(c.name, c.pattern); got != c.want {
			t.Errorf("fnmatchcase(%q, %q) = %v", c.name, c.pattern, got)
		}
	}
}

func TestSearchCapIsVisibleEvenWithoutMatches(t *testing.T) {
	_, root := newTools(t)
	for i := 0; i < 3; i++ {
		writeFile(t, filepath.Join(root, string(rune('0'+i))+".txt"), "nothing")
	}
	ft, err := NewFileTools(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"glob", "*.py"}, {"grep", "no match"}} {
		got := mustExec(t, ft, c[0], map[string]any{"pattern": c[1]}).Output
		if !strings.Contains(got, "scan truncated after 2 files") {
			t.Errorf("%s = %q", c[0], got)
		}
	}
}

func TestTraversalAndAbsoluteEscapeAreRejected(t *testing.T) {
	ft, root := newTools(t)
	for _, p := range []string{"../outside.txt", filepath.Join(filepath.Dir(root), "outside.txt")} {
		if _, err := ft.Execute("write_file", map[string]any{"path": p, "content": "bad"}); err == nil {
			t.Errorf("write to %q succeeded", p)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "outside.txt")); err == nil {
		t.Fatal("file written outside the project")
	}
}

func TestWritesCannotModifyGitOrOrchestraMetadata(t *testing.T) {
	ft, root := newTools(t)
	for _, dir := range []string{".git", ".orchestra", "nested/.git"} {
		target := filepath.Join(root, filepath.FromSlash(dir), "config")
		writeFile(t, target, "original")
		for _, c := range []struct {
			tool string
			args map[string]any
		}{{"write_file", map[string]any{"content": "bad"}}, {"edit_file", map[string]any{"old": "original", "new": "bad"}}} {
			args := map[string]any{"path": target}
			for k, v := range c.args {
				args[k] = v
			}
			if _, err := ft.Execute(c.tool, args); err == nil {
				t.Errorf("%s in %s succeeded", c.tool, dir)
			}
			if readText(t, target) != "original" {
				t.Fatalf("%s changed", target)
			}
		}
	}
	if _, err := ft.Execute("write_file", map[string]any{"path": ".git/hooks/pre-commit", "content": "bad"}); err == nil {
		t.Fatal("write to .git/hooks succeeded")
	}
	variants := []string{".GIT/x", "a/../.git/x"}
	if runtime.GOOS == "windows" {
		variants = append(variants, ".git./x", ".git /x")
	}
	for _, v := range variants {
		if _, err := ft.Execute("write_file", map[string]any{"path": v, "content": "bad"}); err == nil {
			t.Errorf("write to %q succeeded", v)
		}
	}
}

func TestHardlinkAliasCannotModifyProtectedMetadata(t *testing.T) {
	ft, root := newTools(t)
	protected := filepath.Join(root, ".git", "config")
	writeFile(t, protected, "original")
	if err := os.Link(protected, filepath.Join(root, "ordinary.txt")); err != nil {
		t.Skip("filesystem does not support hardlinks")
	}
	for _, c := range []map[string]any{{"content": "bad"}, {"old": "original", "new": "bad"}} {
		name := "write_file"
		if _, ok := c["old"]; ok {
			name = "edit_file"
		}
		c["path"] = "ordinary.txt"
		if _, err := ft.Execute(name, c); err == nil {
			t.Errorf("%s through hardlink succeeded", name)
		}
		if readText(t, protected) != "original" {
			t.Fatal("protected file changed")
		}
	}
}

func TestReadFileLineRanges(t *testing.T) {
	ft, root := newTools(t)
	var b strings.Builder
	for i := 0; i < 8000; i++ {
		fmt.Fprintf(&b, "line-%05d%s", i, "\n")
	}
	writeFile(t, filepath.Join(root, "large.py"), b.String())
	got := mustExec(t, ft, "read_file", jsonArgs(t, `{"path":"large.py","offset":7000,"limit":3}`)).Output
	for _, want := range []string{"line-07000", "line-07002", "997 lines remain"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "line-07003") {
		t.Errorf("read past limit: %q", got)
	}
	for _, bad := range []string{`{"offset":-1}`, `{"limit":0}`, `{"offset":true}`, `{"limit":2.5}`, `{"limit":2001}`} {
		args := jsonArgs(t, bad)
		args["path"] = "large.py"
		if _, err := ft.Execute("read_file", args); err == nil {
			t.Errorf("read_file %s succeeded", bad)
		}
	}
}

func TestSymlinkEscapeIsNotReadOrSearched(t *testing.T) {
	ft, root := newTools(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "secret")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("creating symlinks requires permission on this machine")
	}
	if _, err := ft.Execute("read_file", map[string]any{"path": "link/secret.txt"}); err == nil {
		t.Fatal("read through symlink succeeded")
	}
	if got := mustExec(t, ft, "grep", map[string]any{"pattern": "secret"}).Output; strings.Contains(got, "secret") {
		t.Fatalf("grep found %q", got)
	}
}

func TestDanglingSymlinkCannotWriteOutside(t *testing.T) {
	ft, root := newTools(t)
	outside := filepath.Join(t.TempDir(), "new.txt")
	if err := os.Symlink(outside, filepath.Join(root, "dangling")); err != nil {
		t.Skip("creating symlinks requires permission on this machine")
	}
	if _, err := ft.Execute("write_file", map[string]any{"path": "dangling", "content": "x"}); err == nil {
		t.Fatal("write through dangling symlink succeeded")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("file created outside the project")
	}
}

func TestBadArgumentsAndLargeReadsAreBounded(t *testing.T) {
	ft, root := newTools(t)
	writeFile(t, filepath.Join(root, "large"), strings.Repeat("x", 20000))
	if got := mustExec(t, ft, "read_file", map[string]any{"path": "large"}).Output; len(got) >= 17000 {
		t.Fatalf("read returned %d characters", len(got))
	}
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"read_file", map[string]any{"path": 4}},
		{"edit_file", map[string]any{"path": "large", "old": "", "new": "x"}},
		{"unknown", map[string]any{}},
	} {
		if _, err := ft.Execute(c.name, c.args); err == nil {
			t.Errorf("%s %v succeeded", c.name, c.args)
		}
	}
}

func TestBinaryAndOversizeFilesAreRefused(t *testing.T) {
	ft, root := newTools(t)
	writeFile(t, filepath.Join(root, "bin"), "a\x00b")
	writeFile(t, filepath.Join(root, "huge"), strings.Repeat("x", FileLimit+1))
	for _, p := range []string{"bin", "huge"} {
		if _, err := ft.Execute("read_file", map[string]any{"path": p}); err == nil {
			t.Errorf("read %s succeeded", p)
		}
	}
	if _, err := ft.Execute("write_file", map[string]any{"path": "w", "content": strings.Repeat("x", FileLimit+1)}); err == nil {
		t.Error("oversize write succeeded")
	}
}

func TestWindowsDevicesAndStreamPathsAreRefused(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows device names and junctions")
	}
	ft, _ := newTools(t)
	for _, p := range []string{"NUL", "NUL.txt", "a.txt:secret", `\\?\C:\x`, `\\.\NUL`, "sub/COM1"} {
		if _, err := ft.Execute("write_file", map[string]any{"path": p, "content": "bad"}); err == nil {
			t.Errorf("write to %q succeeded", p)
		}
	}
}

func TestJunctionEscapeIsRefusedWithoutSymlinkPrivileges(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows junction")
	}
	ft, root := newTools(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "secret")
	out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", filepath.Join(root, "junction"), outside).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	if _, err := ft.Execute("read_file", map[string]any{"path": "junction/secret.txt"}); err == nil {
		t.Fatal("read through junction succeeded")
	}
	if _, err := ft.Execute("write_file", map[string]any{"path": "junction/new.txt", "content": "x"}); err == nil {
		t.Fatal("write through junction succeeded")
	}
	if got := mustExec(t, ft, "grep", map[string]any{"pattern": "secret"}).Output; strings.Contains(got, "secret") {
		t.Fatalf("grep found %q", got)
	}
	if got := mustExec(t, ft, "list_dir", map[string]any{}).Output; got != "junction/" {
		t.Fatalf("list_dir = %q", got)
	}
}

func TestPolicyModesGateWritesAndCommands(t *testing.T) {
	for _, c := range []struct {
		mode           string
		write, command bool
	}{{"read-only", true, true}, {"edit", false, true}, {"full", false, false}} {
		if got, _ := NeedsPermission(c.mode, "read_file"); got {
			t.Errorf("%s read_file asks", c.mode)
		}
		if got, _ := NeedsPermission(c.mode, "write_file"); got != c.write {
			t.Errorf("%s write_file = %v", c.mode, got)
		}
		if got, _ := NeedsPermission(c.mode, "run_command"); got != c.command {
			t.Errorf("%s run_command = %v", c.mode, got)
		}
	}
	if _, err := NeedsPermission("invalid", "write_file"); err == nil {
		t.Error("invalid mode accepted")
	}
	if _, err := NeedsPermission("edit", "invented"); err == nil {
		t.Error("unknown tool accepted")
	}
}

func TestSplitLinesMatchesPython(t *testing.T) {
	got := splitLines("a\r\nb\rc\x0bd\u2028e\n", false)
	want := []string{"a", "b", "c", "d", "e"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("splitLines = %q", got)
	}
	if got := splitLines("a\nb", true); len(got) != 2 || got[0] != "a\n" {
		t.Fatalf("keepends = %q", got)
	}
}

func TestSchemasAreFreshAndComplete(t *testing.T) {
	s := Schemas()
	if len(s) != 7 {
		t.Fatalf("%d tools", len(s))
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"read_file", "list_dir", "glob", "grep", "write_file", "edit_file", "run_command", `"exclusiveMinimum":0`} {
		if !strings.Contains(string(b), name) {
			t.Errorf("schema missing %s", name)
		}
	}
}

func mklinkJ(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
}

func TestDanglingJunctionsCannotRedirectWritesOutside(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows junction")
	}
	ft, root := newTools(t)
	outside := t.TempDir()
	// A junction to a missing outside folder.
	mklinkJ(t, filepath.Join(root, "dangling"), filepath.Join(outside, "missing"))
	// A junction to a missing folder inside the project whose parent is a junction
	// to the outside: lexically inside, physically outside.
	mklinkJ(t, filepath.Join(root, "b"), outside)
	mklinkJ(t, filepath.Join(root, "a"), filepath.Join(root, "b", "c"))
	for _, p := range []string{"dangling/x.txt", "a/x.txt", "a"} {
		if _, err := ft.Execute("write_file", map[string]any{"path": p, "content": "x"}); err == nil {
			t.Errorf("write to %q succeeded", p)
		}
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatalf("outside folder now has %d entries", len(entries))
	}
}

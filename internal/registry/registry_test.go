package registry

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Adeel0-0/agent-orchestra/internal/players"
	"github.com/Adeel0-0/agent-orchestra/internal/runner"
)

func TestPlayersInPythonOrder(t *testing.T) {
	if got := IDs(); !slices.Equal(got[:3], []string{"codex", "antigravity", "claude"}) {
		t.Fatalf("ids %v", got)
	}
	spec, err := Get("claude")
	if err != nil || spec.LoginHint != "claude  (then /login)" || !slices.Equal(spec.LoginCheck, []string{"claude", "auth", "status"}) {
		t.Fatalf("claude spec %+v %v", spec, err)
	}
	if agy, _ := Get("antigravity"); agy.LoginCheck != nil {
		t.Fatal("agy has no login check")
	}
	p, err := spec.New(players.Options{Cwd: t.TempDir(), Permission: "read-only"})
	if err != nil || p.ID() != "claude" {
		t.Fatalf("New: %v %v", p, err)
	}
	if _, err := spec.New(players.Options{Cwd: t.TempDir(), Permission: "yolo"}); err == nil {
		t.Fatal("bad permission accepted")
	}
}

func TestGetUnknown(t *testing.T) {
	_, err := Get("nope")
	if err == nil || !strings.HasPrefix(err.Error(), "Unknown player 'nope'. Known: codex, antigravity, claude") {
		t.Fatalf("err %v", err)
	}
}

func TestLoggedInParsesClaudeJSON(t *testing.T) {
	cases := []struct {
		res  runner.Result
		want bool
	}{
		{runner.Result{ExitCode: 0, Stdout: `{"loggedIn": true, "authMethod": "claude.ai"}`}, true},
		{runner.Result{ExitCode: 0, Stdout: "{\"loggedIn\": false}\n"}, false},
		{runner.Result{ExitCode: 0, Stdout: "Logged in using ChatGPT"}, true},
		{runner.Result{ExitCode: 0, Stdout: "Not logged in"}, false},
		{runner.Result{ExitCode: 0, Stderr: "You are not logged in."}, false},
		{runner.Result{ExitCode: 1, Stdout: `{"loggedIn": true}`}, false},
		{runner.Result{ExitCode: -1, Stderr: "Command timed out after 30 seconds."}, false},
		{runner.Result{ExitCode: 0, Stdout: `{"other": 1}`}, true},
	}
	for _, c := range cases {
		if got := LoggedIn(c.res); got != c.want {
			t.Errorf("LoggedIn(%+v) = %v, want %v", c.res, got, c.want)
		}
	}
}

func TestReady(t *testing.T) {
	if (Health{}).Ready() || !(Health{Path: "x"}).Ready() || (Health{Path: "x", LoggedIn: Bool(false)}).Ready() ||
		!(Health{Path: "x", LoggedIn: Bool(true)}).Ready() {
		t.Fatal("ready rules")
	}
	m := Health{ID: "codex", Name: "Codex"}.ToMap()
	if m["path"] != nil || m["logged_in"] != nil || m["ready"] != false || m["id"] != "codex" {
		t.Fatalf("map %v", m)
	}
}

func TestCheckNotInstalled(t *testing.T) {
	h := Check(Spec{ID: "x", DisplayName: "X", Executable: "definitely-not-a-real-cli-xyz", InstallHint: "install x",
		LoginCheck: []string{"definitely-not-a-real-cli-xyz", "status"}})
	if h.Path != "" || h.LoggedIn != nil || h.Fix != "install x" || h.Ready() {
		t.Fatalf("%+v", h)
	}
}

func TestCheckHealthOverride(t *testing.T) {
	h := Check(Spec{ID: "openrouter", Health: func() Health { return Health{ID: "openrouter", Path: "https://x", LoggedIn: Bool(true)} }})
	if !h.Ready() || h.ID != "openrouter" {
		t.Fatalf("%+v", h)
	}
}

// writeFakeCLI puts an executable named name on a PATH of its own. It prints a version
// for --version and loginOutput for anything else.
func writeFakeCLI(t *testing.T, name, loginOutput string) {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		script := "@echo off\r\nif \"%1\"==\"--version\" (\r\n  echo fake 1.2.3\r\n  exit /b 0\r\n)\r\necho " + loginOutput + "\r\nexit /b 0\r\n"
		if err := os.WriteFile(filepath.Join(dir, name+".cmd"), []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'fake 1.2.3'; exit 0; fi\necho '" + loginOutput + "'\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestCheckWithFakeCLI(t *testing.T) {
	spec := Spec{ID: "fake", DisplayName: "Fake", Executable: "fakeclaude", InstallHint: "install",
		LoginHint: "fakeclaude login", LoginCheck: []string{"fakeclaude", "auth", "status"}}

	writeFakeCLI(t, "fakeclaude", `{"loggedIn": false, "authMethod": "none"}`)
	h := Check(spec)
	if h.Path == "" || h.Version != "fake 1.2.3" || h.LoggedIn == nil || *h.LoggedIn || h.Fix != "fakeclaude login" || h.Ready() {
		t.Fatalf("logged out: %+v", h)
	}

	writeFakeCLI(t, "fakeclaude", `{"loggedIn": true}`)
	h = Check(spec)
	if h.LoggedIn == nil || !*h.LoggedIn || h.Fix != "" || !h.Ready() {
		t.Fatalf("logged in: %+v", h)
	}

	spec.LoginCheck = nil // like agy: installed, login not checkable
	h = Check(spec)
	if h.LoggedIn != nil || !h.Ready() {
		t.Fatalf("no login check: %+v", h)
	}
}

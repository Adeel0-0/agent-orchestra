package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Adeel0-0/agent-orchestra/internal/config"
	"github.com/Adeel0-0/agent-orchestra/internal/model"
)

var goBinary string // built once in TestMain

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "orchestra-bin-")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	goBinary = filepath.Join(dir, "orchestra")
	if runtime.GOOS == "windows" {
		goBinary += ".exe"
	}
	build := exec.Command("go", "build", "-o", goBinary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Printf("go build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type result struct {
	stdout, stderr string
	code           int
	took           time.Duration
}

func runIn(t *testing.T, dir string, env []string, name string, args ...string) result {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	start := time.Now()
	err := cmd.Run()
	r := result{stdout: out.String(), stderr: errOut.String(), took: time.Since(start)}
	if exitErr, ok := err.(*exec.ExitError); ok {
		r.code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return r
}

func goCLI(t *testing.T, dir string, args ...string) result {
	t.Helper()
	return runIn(t, dir, nil, goBinary, args...)
}

// ---------------------------------------------------------------- Go CLI only

func TestCLIMessagesAndExitCodes(t *testing.T) {
	dir := t.TempDir()
	if r := goCLI(t, dir, "claim", "AUTH-001", "--agent", "claude", "--scope", "OAuth2"); r.code != 0 ||
		r.stdout != "[OK] Agent 'claude' claimed writer lock for task 'AUTH-001'.\n" {
		t.Fatalf("claim: %+v", r)
	}
	if r := goCLI(t, dir, "claim", "X", "--agent", "codex"); r.code != 1 ||
		!strings.HasPrefix(r.stderr, "[FAILED] Cannot acquire lock: held by agent 'claude' for task 'AUTH-001'.") {
		t.Fatalf("refused claim: %+v", r)
	}
	if r := goCLI(t, dir, "release", "--agent", "codex"); r.code != 1 || !strings.Contains(r.stderr, "held by 'claude', not 'codex'") {
		t.Fatalf("refused release: %+v", r)
	}
	if r := goCLI(t, dir, "status"); r.code != 0 || !strings.Contains(r.stdout, "Is Locked:    True") || !strings.Contains(r.stdout, "  Agent:      claude") {
		t.Fatalf("status: %+v", r)
	}
	if r := goCLI(t, dir, "--agent", "x"); r.code != 2 {
		t.Fatalf("bad command should be a usage error: %+v", r)
	}
	if r := goCLI(t, dir, "release", "--status", "bogus"); r.code != 2 {
		t.Fatalf("bad choice should be a usage error: %+v", r)
	}
	if r := goCLI(t, dir, "release", "--agent", "claude", "--status", "paused", "--summary", "wip"); r.code != 0 ||
		r.stdout != "[OK] Writer lock released for task 'AUTH-001'. Event emitted.\n" {
		t.Fatalf("release: %+v", r)
	}
	if r := goCLI(t, dir, "emit", "REVIEW_VERDICT", `{"accepted": true}`, "--agent", "codex"); r.code != 0 ||
		!strings.Contains(r.stdout, "(REVIEW_VERDICT)") {
		t.Fatalf("emit: %+v", r)
	}
	if r := goCLI(t, dir, "emit", "X", "{bad"); r.code != 1 || !strings.HasPrefix(r.stderr, "[FAILED] Invalid JSON payload:") {
		t.Fatalf("bad payload: %+v", r)
	}

	r := goCLI(t, dir, "status", "--json")
	var st struct {
		State       string           `json:"state"`
		IsLocked    bool             `json:"is_locked"`
		LatestEvent model.AgentEvent `json:"latest_event"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil || st.IsLocked || st.LatestEvent.EventType != model.ReviewVerdict {
		t.Fatalf("status --json: %v %s", err, r.stdout)
	}
}

func TestInitAndConfigErrors(t *testing.T) {
	dir := t.TempDir()
	if r := goCLI(t, dir, "init"); r.code != 0 || r.stdout != "[OK] Created orchestra.json with starter defaults.\n" {
		t.Fatalf("init: %+v", r)
	}
	if r := goCLI(t, dir, "init"); r.code != 1 || !strings.Contains(r.stdout, "already exists") {
		t.Fatalf("second init: %+v", r)
	}
	if r := goCLI(t, dir, "init", "--force"); r.code != 0 {
		t.Fatalf("init --force: %+v", r)
	}
	if _, err := config.Load(dir); err != nil {
		t.Fatalf("init wrote an unloadable config: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "orchestra.json"), []byte(`{"git": {`), 0o644)
	// Phase 5 is halted: a broken file is exit 1 with a message, not Python's exit 2.
	if r := goCLI(t, dir, "status"); r.code != 1 || !strings.HasPrefix(r.stderr, "[FAILED] Invalid configuration:") {
		t.Fatalf("broken config: %+v", r)
	}
	os.WriteFile(filepath.Join(dir, "orchestra.json"), []byte(`{"unknown_key": true}`), 0o644)
	if r := goCLI(t, dir, "status"); r.code != 0 {
		t.Fatalf("unknown keys must be accepted while phase 5 is halted: %+v", r)
	}
}

func TestFlagsMayFollowPositionalsAndRunPassesArgsThrough(t *testing.T) {
	p, err := parseArgs([]string{"T-1", "--agent=codex", "--lease", "60"}, []flagSpec{{Name: "agent", Kind: "string"}, {Name: "lease", Kind: "int"}}, -1)
	if err != nil || p.positional[0] != "T-1" || p.str("agent") != "codex" || p.num("lease") != 60 {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = parseArgs([]string{"script.py", "--verbose", "x"}, nil, 1)
	if err != nil || strings.Join(p.positional, " ") != "script.py --verbose x" {
		t.Fatalf("run args: %+v %v", p, err)
	}
}

package main

import (
	"bufio"
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

	"github.com/Adeel0-0/agent-orchestra/internal/bus"
	"github.com/Adeel0-0/agent-orchestra/internal/config"
	"github.com/Adeel0-0/agent-orchestra/internal/daemon"
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

// ------------------------------------------------------- Go <-> Python checks

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// pythonCLI runs the reference Python CLI (orchestra/ at the repo root) in dir.
func pythonCLI(t *testing.T, dir string, args ...string) result {
	t.Helper()
	py := pythonOrSkip(t)
	all := append([]string{"-c", "import sys; from orchestra.cli import main; sys.exit(main(sys.argv[1:]))"}, args...)
	return runIn(t, dir, []string{"PYTHONPATH=" + repoRoot(t), "PYTHONIOENCODING=utf-8"}, py, all...)
}

func pythonOrSkip(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python", "python3"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("python is not on PATH; cross-language tests skipped")
	return ""
}

func TestPythonHeldLockRefusesGoClaimAndViceVersa(t *testing.T) {
	dir := t.TempDir()
	if r := pythonCLI(t, dir, "claim", "PY-1", "--agent", "py-agent"); r.code != 0 {
		t.Fatalf("python claim: %+v", r)
	}
	if r := goCLI(t, dir, "claim", "GO-1", "--agent", "go-agent"); r.code != 1 || !strings.Contains(r.stderr, "held by agent 'py-agent'") {
		t.Fatalf("Go claimed over Python's lock: %+v", r)
	}
	if r := goCLI(t, dir, "release", "--agent", "py-agent"); r.code != 0 {
		t.Fatalf("Go could not release Python's lock for its owner: %+v", r)
	}
	if r := goCLI(t, dir, "claim", "GO-1", "--agent", "go-agent"); r.code != 0 {
		t.Fatalf("go claim: %+v", r)
	}
	if r := pythonCLI(t, dir, "claim", "PY-2", "--agent", "py-agent"); r.code != 1 || !strings.Contains(r.stderr, "held by agent 'go-agent'") {
		t.Fatalf("Python claimed over Go's lock: %+v", r)
	}
	if r := pythonCLI(t, dir, "status", "--json"); r.code != 0 || !strings.Contains(r.stdout, `"agent_id": "go-agent"`) {
		t.Fatalf("Python cannot read Go's writer.lock: %+v", r)
	}
}

const holdGuardPy = `
import os, sys, time
path, hold = sys.argv[1], float(sys.argv[2])
fh = open(path, "a+b")
fh.seek(0)
if os.name == "nt":
    import msvcrt
    while True:
        try:
            msvcrt.locking(fh.fileno(), msvcrt.LK_NBLCK, 1)
            break
        except OSError:
            time.sleep(0.01)
else:
    import fcntl
    fcntl.flock(fh.fileno(), fcntl.LOCK_EX)
print("held", flush=True)
time.sleep(hold)
fh.seek(0)
if os.name == "nt":
    msvcrt.locking(fh.fileno(), msvcrt.LK_UNLCK, 1)
else:
    fcntl.flock(fh.fileno(), fcntl.LOCK_UN)
`

func TestPythonGuardBlocksGoClaimUntilReleased(t *testing.T) {
	py := pythonOrSkip(t)
	dir := t.TempDir()
	guard := filepath.Join(dir, ".orchestra", "writer.lock.guard")
	os.MkdirAll(filepath.Dir(guard), 0o755)
	holder := exec.Command(py, "-c", holdGuardPy, guard, "2")
	stdout, _ := holder.StdoutPipe()
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer holder.Wait()
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); strings.TrimSpace(line) != "held" {
		t.Fatalf("python guard holder said %q", line)
	}
	r := goCLI(t, dir, "claim", "T", "--agent", "go-agent")
	if r.code != 0 {
		t.Fatalf("claim after guard release failed: %+v", r)
	}
	if r.took < 1500*time.Millisecond {
		t.Fatalf("Go claimed in %v while Python held the guard byte", r.took)
	}
}

func TestGoGuardBlocksPythonClaimUntilReleased(t *testing.T) {
	pythonOrSkip(t)
	dir := t.TempDir()
	locks, _ := bus.NewLockManager(filepath.Join(dir, ".orchestra"))
	held, release := make(chan struct{}), make(chan struct{})
	go locks.WithGuard(func() error {
		close(held)
		<-release
		return nil
	})
	<-held
	go func() { time.Sleep(2 * time.Second); close(release) }()
	r := pythonCLI(t, dir, "claim", "T", "--agent", "py-agent")
	if r.code != 0 {
		t.Fatalf("python claim after guard release failed: %+v", r)
	}
	if r.took < 1500*time.Millisecond {
		t.Fatalf("Python claimed in %v while Go held the guard byte", r.took)
	}
}

const readEventsPy = `
import json, sys
from orchestra.bus import EventBus
b = EventBus(sys.argv[1])
events, offset = b.read_from(0)
print(json.dumps({"read_events": [[e.event_type.value, e.source_agent, e.payload] for e in b.read_events(limit=1000)],
                  "read_from": len(events), "offset": offset}))
`

func TestPythonParsesEventsWrittenByGo(t *testing.T) {
	py := pythonOrSkip(t)
	dir := t.TempDir()
	goCLI(t, dir, "claim", "T", "--agent", "go-agent")
	goCLI(t, dir, "release", "--agent", "go-agent")
	goCLI(t, dir, "emit", "NOT_A_TYPE", `{"text": "naïve, \"quoted\": ok"}`, "--agent", "go-agent")

	r := runIn(t, dir, []string{"PYTHONPATH=" + repoRoot(t)}, py, "-c", readEventsPy, filepath.Join(dir, ".orchestra"))
	var got struct {
		ReadEvents [][]any `json:"read_events"`
		ReadFrom   int     `json:"read_from"`
		Offset     int64   `json:"offset"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("python output %q / %q: %v", r.stdout, r.stderr, err)
	}
	if len(got.ReadEvents) != 3 || got.ReadFrom != 3 {
		t.Fatalf("python saw %+v", got)
	}
	if got.ReadEvents[0][0] != "AGENT_CLAIM" || got.ReadEvents[1][0] != "HANDOFF_REQUEST" || got.ReadEvents[2][0] != "CUSTOM_EVENT" {
		t.Fatalf("types %+v", got.ReadEvents)
	}
	released := got.ReadEvents[1][2].(map[string]any)["released_lock"].(map[string]any)
	if released["agent_id"] != "go-agent" {
		t.Fatalf("released_lock lost: %+v", got.ReadEvents[1])
	}
	if got.ReadEvents[2][2].(map[string]any)["text"] != `naïve, "quoted": ok` {
		t.Fatalf("payload text %+v", got.ReadEvents[2])
	}
	info, _ := os.Stat(filepath.Join(dir, ".orchestra", "events.jsonl"))
	if got.Offset != info.Size() {
		t.Fatalf("python read_from offset %d, file size %d", got.Offset, info.Size())
	}
}

// listenUntil runs a `listen --all` process until its output has want lines.
func listenUntil(t *testing.T, cmd *exec.Cmd, want int) []string {
	t.Helper()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	var got []string
	timeout := time.After(20 * time.Second)
	for len(got) < want {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("listen exited early: %q", got)
			}
			if strings.HasPrefix(l, "[") && !strings.HasPrefix(l, "[LISTEN]") {
				got = append(got, l)
			}
		case <-timeout:
			t.Fatalf("listen printed only %q", got)
		}
	}
	return got
}

func TestPythonListenReadsGoEvents(t *testing.T) {
	py := pythonOrSkip(t)
	dir := t.TempDir()
	goCLI(t, dir, "emit", "REVIEW_REQUEST", `{"task_id": "T-9"}`, "--agent", "go-agent")
	goCLI(t, dir, "emit", "CI_UPDATE", `{"run": 1}`, "--agent", "go-agent")
	cmd := exec.Command(py, "-u", "-c", "import sys; from orchestra.cli import main; sys.exit(main(sys.argv[1:]))", "listen", "--all")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PYTHONPATH="+repoRoot(t), "PYTHONIOENCODING=utf-8")
	got := listenUntil(t, cmd, 2)
	if got[0] != `[REVIEW_REQUEST] (go-agent) -> {"task_id": "T-9"}` || got[1] != `[CI_UPDATE] (go-agent) -> {"run": 1}` {
		t.Fatalf("python listen printed %q", got)
	}
}

func TestGoReadsEventsWrittenByPython(t *testing.T) {
	dir := t.TempDir()
	if r := pythonCLI(t, dir, "claim", "PY-1", "--agent", "py-agent"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	pythonCLI(t, dir, "release", "--agent", "py-agent", "--status", "paused")
	pythonCLI(t, dir, "emit", "WHATEVER", `{"n": 2}`, "--agent", "py-agent")

	b, _ := bus.NewEventBus(filepath.Join(dir, ".orchestra"))
	events, offset, err := b.ReadFrom(0)
	if err != nil || len(events) != 3 || offset != b.EndOffset() {
		t.Fatalf("Go read %d events (offset %d of %d): %v", len(events), offset, b.EndOffset(), err)
	}
	if events[1].EventType != model.HandoffRequest || events[1].Payload["status"] != "paused" || events[2].EventType != model.CustomEvent {
		t.Fatalf("%+v", events)
	}
	if lock, ok := events[1].Payload["released_lock"].(map[string]any); !ok || lock["agent_id"] != "py-agent" {
		t.Fatalf("released_lock from Python: %+v", events[1].Payload)
	}

	cmd := exec.Command(goBinary, "listen", "--all")
	cmd.Dir = dir
	got := listenUntil(t, cmd, 3)
	if got[0] != `[AGENT_CLAIM] (py-agent) -> {"scope": "", "task_id": "PY-1"}` || !strings.HasPrefix(got[2], "[CUSTOM_EVENT] (py-agent)") {
		t.Fatalf("go listen printed %q", got)
	}
}

func TestGoDaemonShipsAPythonAgentsHandoff(t *testing.T) {
	pythonOrSkip(t)
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"},
	} {
		runIn(t, repo, nil, "git", args...)
	}
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".orchestra/\n"), 0o644)
	runIn(t, repo, nil, "git", "add", ".")
	runIn(t, repo, nil, "git", "commit", "-q", "-m", "init")

	cfg := config.Default()
	cfg.WorkspaceRoot = repo
	cfg.Git.AutoPush = false
	verify := "git --version"
	cfg.Pipeline.VerifyCommand = &verify
	d, err := daemon.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SaveCursor(); err != nil { // what `start` does before its loop
		t.Fatal(err)
	}

	os.WriteFile(filepath.Join(repo, "feature.py"), []byte("x = 1\n"), 0o644)
	pythonCLI(t, repo, "claim", "PY-1", "--agent", "py-agent")
	if r := pythonCLI(t, repo, "release", "--agent", "py-agent", "--status", "complete"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if _, err := d.Tick(); err != nil {
		t.Fatal(err)
	}
	if r := runIn(t, repo, nil, "git", "log", "--format=%s", "-1"); !strings.Contains(r.stdout, "automated multi-agent handoff sync") {
		t.Fatalf("Go daemon did not ship the Python agent's handoff: %q (state %s)", r.stdout, d.State)
	}
}

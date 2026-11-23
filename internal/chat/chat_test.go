package chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Adeel0-0/agent-orchestra/internal/players"
	"github.com/Adeel0-0/agent-orchestra/internal/registry"
)

func project() string {
	if runtime.GOOS == "windows" {
		return `C:\project`
	}
	return "/project"
}

func noColor() *bool { b := false; return &b }

func replayCodex(t *testing.T, name string) []players.Event {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "tests", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m := players.NewCodexMapper(project())
	var events []players.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(sc.Bytes(), &obj); err != nil {
			t.Fatal(err)
		}
		events = append(events, m.Feed(obj)...)
	}
	return events
}

// ------------------------------------------------------------------ renderer

func TestRendersATurnWithoutColor(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out, noColor(), "", "")
	for _, e := range replayCodex(t, "codex_edit.jsonl") {
		r.Show(e)
	}
	text := out.String()
	for _, want := range []string{"> shell", "~ a.txt (update)", "167 out tokens"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\033[") {
		t.Error("color codes without color")
	}
}

func toolLine(input map[string]any) string {
	var out bytes.Buffer
	NewRenderer(&out, noColor(), "", project()).Show(players.NewEvent(players.ToolStarted,
		map[string]any{"id": "1", "name": "t", "input": input}))
	return strings.TrimRight(out.String(), "\n")
}

func TestToolPathsAreRelativeToTheProject(t *testing.T) {
	// Antigravity's read/edit tools use "Path"; Claude's use "file_path".
	if got := toolLine(map[string]any{"Path": filepath.Join(project(), "calc.py")}); got != "  > t calc.py" {
		t.Errorf("Path: %q", got)
	}
	want := "  > t " + filepath.Join("src", "x.py")
	if got := toolLine(map[string]any{"file_path": filepath.Join(project(), "src", "x.py")}); got != want {
		t.Errorf("file_path: %q", got)
	}
}

func TestLongOutsidePathKeepsTheFileName(t *testing.T) {
	far := filepath.Join(filepath.Dir(project()), strings.Repeat("d", 150), "important.py")
	line := toolLine(map[string]any{"file_path": far})
	if !strings.HasSuffix(line, "important.py") || !strings.Contains(line, "...") {
		t.Fatalf("%q", line)
	}
}

func TestMultiLineCommandStaysOnOneLine(t *testing.T) {
	got := toolLine(map[string]any{"command": "Get-Content calc.py\nGet-ChildItem -Name"})
	if got != "  > t Get-Content calc.py ; Get-ChildItem -Name" {
		t.Fatalf("%q", got)
	}
}

func TestToolSummaryFallbacks(t *testing.T) {
	if got := toolLine(map[string]any{"paths": []any{filepath.Join(project(), "a.txt"), "b.txt"}}); got != "  > t a.txt, b.txt" {
		t.Errorf("paths: %q", got)
	}
	if got := toolLine(map[string]any{"query": "x"}); got != `  > t {"query": "x"}` {
		t.Errorf("json: %q", got)
	}
	if got := toolLine(map[string]any{}); got != "  > t" {
		t.Errorf("empty: %q", got)
	}
}

func TestToolOutputShowsTheLastFourLines(t *testing.T) {
	var out bytes.Buffer
	NewRenderer(&out, noColor(), "", "").Show(players.NewEvent(players.ToolOutput,
		map[string]any{"id": "1", "chunk": "1\n2\n3\n4\n5\n6\n\n"}))
	want := "    | ... 2 more lines\n    | 3\n    | 4\n    | 5\n    | 6\n"
	if out.String() != want {
		t.Fatalf("%q", out.String())
	}
}

func TestAuthErrorShowsFix(t *testing.T) {
	var out bytes.Buffer
	NewRenderer(&out, noColor(), "codex login", "").Show(players.NewEvent(players.Error,
		map[string]any{"message": "not logged in", "kind": "auth"}))
	if !strings.Contains(out.String(), "fix: codex login") {
		t.Fatalf("%q", out.String())
	}
}

func TestTextThenToolBreaksTheLineAndUsageLine(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out, noColor(), "", "")
	r.Show(players.NewEvent(players.TextDelta, map[string]any{"text": "hi"}))
	r.Show(players.NewEvent(players.Notice, map[string]any{"message": "careful"}))
	r.Show(players.NewEvent(players.Usage, map[string]any{"input_tokens": int64(5), "output_tokens": 7.0, "cost_usd": 0.5}))
	r.Show(players.NewEvent(players.TurnCompleted, map[string]any{"stop_reason": "error"}))
	r.Show(players.NewEvent(players.TurnCompleted, map[string]any{"stop_reason": "end_turn"}))
	want := "hi\n  ! careful\n[5 in / 7 out tokens, $0.500000, error]\n\n"
	if out.String() != want {
		t.Fatalf("%q", out.String())
	}
}

func TestColorWrapsText(t *testing.T) {
	var out bytes.Buffer
	on := true
	NewRenderer(&out, &on, "", "").Show(players.NewEvent(players.Notice, map[string]any{"message": "x"}))
	if out.String() != "\033[33m  ! x\033[0m\n" {
		t.Fatalf("%q", out.String())
	}
}

// ------------------------------------------------------------------ chat runs

type fakePlayer struct {
	session string
	turns   []string
	answers []bool
	script  func(ctx context.Context, p *fakePlayer, emit func(players.Event)) error
}

func (p *fakePlayer) ID() string        { return "fake" }
func (p *fakePlayer) SessionID() string { return p.session }
func (p *fakePlayer) AnswerPermission(_ string, allow bool) error {
	p.answers = append(p.answers, allow)
	return nil
}
func (p *fakePlayer) Send(ctx context.Context, prompt string, emit func(players.Event)) error {
	p.turns = append(p.turns, prompt)
	return p.script(ctx, p, emit)
}

func replyScript(text string) func(context.Context, *fakePlayer, func(players.Event)) error {
	return func(_ context.Context, p *fakePlayer, emit func(players.Event)) error {
		p.session = "sess-1"
		emit(players.NewEvent(players.SessionStarted, map[string]any{"backend_session_id": "sess-1", "model": nil}))
		emit(players.NewEvent(players.TextDelta, map[string]any{"text": text}))
		emit(players.NewEvent(players.TurnCompleted, map[string]any{"stop_reason": "end_turn"}))
		return nil
	}
}

type harness struct {
	player         *fakePlayer
	opts           Options
	stdin          string
	stdout, stderr bytes.Buffer
}

func newHarness(t *testing.T, player *fakePlayer, prompt *string) *harness {
	h := &harness{player: player}
	h.opts = Options{
		Agent: "fake", Prompt: prompt, Cwd: t.TempDir(), Color: noColor(), Interactive: noColor(),
		Lookup: func(id string) (registry.Spec, error) {
			if id != "fake" {
				return registry.Spec{}, errors.New("Unknown player '" + id + "'. Known: fake")
			}
			return registry.Spec{ID: "fake", DisplayName: "Fake Player", LoginHint: "fake login",
				New: func(players.Options) (players.Player, error) { return player, nil },
				Health: func() registry.Health {
					return registry.Health{ID: "fake", Path: "/bin/fake", LoggedIn: registry.Bool(true)}
				}}, nil
		},
		Interrupts: make(chan os.Signal),
	}
	return h
}

func (h *harness) run() int {
	return Run(h.opts, strings.NewReader(h.stdin), &h.stdout, &h.stderr)
}

func strp(s string) *string { return &s }

func TestOneShotChat(t *testing.T) {
	h := newHarness(t, &fakePlayer{script: replyScript("hello there\n")}, strp("hi"))
	if code := h.run(); code != 0 {
		t.Fatalf("exit %d stderr %q", code, h.stderr.String())
	}
	if h.stdout.String() != "hello there\n\n" {
		t.Fatalf("stdout %q", h.stdout.String())
	}
	if h.stderr.String() != "session: sess-1  (continue with --resume sess-1)\n" {
		t.Fatalf("stderr %q", h.stderr.String())
	}
	if h.player.turns[0] != "hi" {
		t.Fatalf("prompt %q", h.player.turns)
	}
}

func TestOneShotPromptFromStdin(t *testing.T) {
	h := newHarness(t, &fakePlayer{script: replyScript("ok")}, strp("-"))
	h.stdin = "from stdin & more\n"
	if code := h.run(); code != 0 || h.player.turns[0] != "from stdin & more\n" {
		t.Fatalf("exit %d turns %q", code, h.player.turns)
	}
}

func TestOneShotErrorExitsOne(t *testing.T) {
	h := newHarness(t, &fakePlayer{script: func(_ context.Context, _ *fakePlayer, emit func(players.Event)) error {
		emit(players.NewEvent(players.Error, map[string]any{"message": "expired", "kind": "auth", "retryable": false}))
		emit(players.NewEvent(players.TurnCompleted, map[string]any{"stop_reason": "error"}))
		return nil
	}}, strp("hi"))
	if code := h.run(); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if h.stdout.String() != "error: expired\n  fix: fake login\n[error]\n" {
		t.Fatalf("stdout %q", h.stdout.String())
	}
	if h.stderr.Len() != 0 {
		t.Fatalf("no session, no hint: %q", h.stderr.String())
	}
}

func TestJSONModePrintsEventsAndDeniesApprovals(t *testing.T) {
	player := &fakePlayer{script: func(_ context.Context, p *fakePlayer, emit func(players.Event)) error {
		emit(players.NewEvent(players.PermissionRequested, map[string]any{
			"request_id": "r1", "tool": "write_file", "input": map[string]any{"path": "é.txt"}, "reason": ""}))
		return replyScript("done")(context.Background(), p, emit)
	}}
	h := newHarness(t, player, strp("hi"))
	yes := true
	h.opts.JSON, h.opts.Interactive = true, &yes
	h.stdin = "y\n"
	if code := h.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimRight(h.stdout.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines %q", lines)
	}
	if lines[0] != `{"type": "permission.requested", "input": {"path": "é.txt"}, "reason": "", "request_id": "r1", "tool": "write_file"}` {
		t.Fatalf("first line %q", lines[0])
	}
	for _, l := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(l), &obj); err != nil || obj["type"] == nil {
			t.Fatalf("not an event: %q", l)
		}
	}
	if len(player.answers) != 1 || player.answers[0] {
		t.Fatalf("answers %v", player.answers)
	}
	if h.stderr.Len() != 0 {
		t.Fatalf("json mode prints no session hint: %q", h.stderr.String())
	}
}

func TestInteractiveApproval(t *testing.T) {
	player := &fakePlayer{script: func(_ context.Context, _ *fakePlayer, emit func(players.Event)) error {
		emit(players.NewEvent(players.PermissionRequested, map[string]any{
			"request_id": "r1", "tool": "shell", "input": map[string]any{"command": "ls"}}))
		emit(players.NewEvent(players.TurnCompleted, map[string]any{"stop_reason": "end_turn"}))
		return nil
	}}
	h := newHarness(t, player, strp("hi"))
	yes := true
	h.opts.Interactive = &yes
	h.stdin = "Yes\n"
	h.run()
	if len(player.answers) != 1 || !player.answers[0] {
		t.Fatalf("answers %v", player.answers)
	}
	if !strings.Contains(h.stdout.String(), `  ? shell: {"command": "ls"}`+"\nAllow this action? [y/N] ") {
		t.Fatalf("stdout %q", h.stdout.String())
	}
}

func TestNonInteractiveApprovalDenies(t *testing.T) {
	player := &fakePlayer{script: func(_ context.Context, _ *fakePlayer, emit func(players.Event)) error {
		emit(players.NewEvent(players.PermissionRequested, map[string]any{"request_id": "r1", "tool": "shell"}))
		return nil
	}}
	h := newHarness(t, player, strp("hi"))
	h.stdin = "y\n"
	h.run()
	if len(player.answers) != 1 || player.answers[0] || strings.Contains(h.stdout.String(), "Allow") {
		t.Fatalf("answers %v stdout %q", player.answers, h.stdout.String())
	}
}

func TestInteractiveConversation(t *testing.T) {
	player := &fakePlayer{script: replyScript("pong\n")}
	h := newHarness(t, player, nil)
	h.opts.Permission = "read-only"
	h.stdin = "/session\n\nping\n/session\n/exit\nnever\n"
	if code := h.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := "Fake Player in " + h.opts.Cwd + "  [read-only]  /exit to quit\n" +
		"\n> (none yet)\n" +
		"\n> " +
		"\n> pong\n\n" +
		"\n> sess-1\n" +
		"\n> "
	if h.stdout.String() != want {
		t.Fatalf("stdout\n%q\nwant\n%q", h.stdout.String(), want)
	}
	if len(player.turns) != 1 || player.turns[0] != "ping" {
		t.Fatalf("turns %q", player.turns)
	}
}

func TestInteractiveEOFExits(t *testing.T) {
	h := newHarness(t, &fakePlayer{script: replyScript("x")}, nil)
	h.stdin = ""
	if code := h.run(); code != 0 || !strings.HasSuffix(h.stdout.String(), "\n> \n") {
		t.Fatalf("exit %d stdout %q", code, h.stdout.String())
	}
}

func TestCtrlCStopsTheTurnThenExitsAtThePrompt(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	player := &fakePlayer{script: func(ctx context.Context, _ *fakePlayer, emit func(players.Event)) error {
		emit(players.NewEvent(players.TextDelta, map[string]any{"text": "working"}))
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}}
	h := newHarness(t, player, nil)
	sig := make(chan os.Signal)
	h.opts.Interrupts = sig
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdinR.Close()
	result := make(chan int, 1)
	go func() { result <- Run(h.opts, stdinR, &h.stdout, &h.stderr) }()

	stdinW.WriteString("go\n")
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("turn never started")
	}
	sig <- os.Interrupt // during the turn: stop it
	<-cancelled
	sig <- os.Interrupt // at the prompt: exit
	select {
	case code := <-result:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("chat did not exit")
	}
	stdinW.Close()
	if !strings.Contains(h.stdout.String(), "working\n[stopped]\n\n> \n") {
		t.Fatalf("stdout %q", h.stdout.String())
	}
}

func TestOneShotCtrlCExits130(t *testing.T) {
	player := &fakePlayer{script: func(ctx context.Context, p *fakePlayer, _ func(players.Event)) error {
		p.session = "s9"
		<-ctx.Done()
		return ctx.Err()
	}}
	h := newHarness(t, player, strp("hi"))
	sig := make(chan os.Signal, 1)
	sig <- os.Interrupt
	h.opts.Interrupts = sig
	if code := h.run(); code != 130 {
		t.Fatalf("exit %d", code)
	}
	if h.stdout.String() != "[stopped]\n" || !strings.Contains(h.stderr.String(), "--resume s9") {
		t.Fatalf("stdout %q stderr %q", h.stdout.String(), h.stderr.String())
	}
}

func TestNotReadyAndUnknownAgent(t *testing.T) {
	h := newHarness(t, &fakePlayer{script: replyScript("x")}, strp("hi"))
	lookup := h.opts.Lookup
	h.opts.Lookup = func(id string) (registry.Spec, error) {
		spec, err := lookup(id)
		spec.Health = func() registry.Health { return registry.Health{Fix: "install fake"} }
		return spec, err
	}
	if code := h.run(); code != 2 || h.stderr.String() != "Fake Player is not ready: not installed.\n  fix: install fake\n" {
		t.Fatalf("exit %d stderr %q", code, h.stderr.String())
	}

	h = newHarness(t, &fakePlayer{script: replyScript("x")}, strp("hi"))
	h.opts.Agent = "nope"
	if code := h.run(); code != 2 || !strings.HasPrefix(h.stderr.String(), "[FAILED] Unknown player 'nope'") {
		t.Fatalf("exit %d stderr %q", code, h.stderr.String())
	}

	h = newHarness(t, &fakePlayer{script: replyScript("x")}, strp("hi"))
	h.opts.Cwd = filepath.Join(h.opts.Cwd, "missing")
	if code := h.run(); code != 2 || !strings.HasPrefix(h.stderr.String(), "[FAILED] Not a folder: ") {
		t.Fatalf("exit %d stderr %q", code, h.stderr.String())
	}
}

func TestRealRegistryRejectsBadPermission(t *testing.T) {
	code := Run(Options{Agent: "codex", Prompt: strp("hi"), Cwd: t.TempDir(), Permission: "yolo"},
		strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if code != 2 {
		t.Fatalf("exit %d", code)
	}
}

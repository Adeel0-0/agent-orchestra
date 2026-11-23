package players

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Ported from tests/test_players.py. Fixtures are read from the Python test tree.

var fixtures = filepath.Join("..", "..", "tests", "fixtures")

func project() string {
	if runtime.GOOS == "windows" {
		return `C:\project`
	}
	return "/project"
}

func readFixtureLines(t *testing.T, name string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var objs []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		objs = append(objs, obj)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return objs
}

func replay(t *testing.T, m Mapper, name string) []Event {
	t.Helper()
	var events []Event
	for _, obj := range readFixtureLines(t, name) {
		events = append(events, m.Feed(obj)...)
	}
	return events
}

func types(events []Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func count(events []Event, typ string) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func ofType(events []Event, typ string) []Event {
	var out []Event
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func textOf(events []Event) string {
	var b strings.Builder
	for _, e := range ofType(events, TextDelta) {
		b.WriteString(e.Str("text"))
	}
	return b.String()
}

func input(e Event) map[string]any {
	m, _ := e.Data["input"].(map[string]any)
	return m
}

// ------------------------------------------------------------------ codex

func TestCodexEditTurn(t *testing.T) {
	m := NewCodexMapper(project())
	ev := replay(t, m, "codex_edit.jsonl")
	if m.SessionID() != "01a1d9a8-e58b-7e22-8a89-cbe634014e35" {
		t.Fatalf("session %q", m.SessionID())
	}
	if ev[0].Type != SessionStarted {
		t.Fatalf("first event %s", ev[0].Type)
	}
	if got := types(ev)[len(ev)-2:]; !slices.Equal(got, []string{Usage, TurnCompleted}) {
		t.Fatalf("last events %v", got)
	}
	text := textOf(ev)
	if !strings.Contains(text, "goodbye") || !strings.Contains(text, "\n\n") {
		t.Fatalf("text %q", text)
	}

	var shell Event
	for _, e := range ofType(ev, ToolStarted) {
		if e.Data["name"] == "shell" {
			shell = e
			break
		}
	}
	if input(shell)["command"] != "cat a.txt" {
		t.Fatalf("shell input %v", input(shell))
	}
	if raw, _ := input(shell)["raw_command"].(string); !strings.Contains(strings.ToLower(raw), "powershell") {
		t.Fatalf("raw_command %q", raw)
	}
	if out := ofType(ev, ToolOutput)[0].Str("chunk"); strings.TrimSpace(out) != "hello" {
		t.Fatalf("output %q", out)
	}
	changed := ofType(ev, FileChanged)
	if len(changed) != 1 || changed[0].Str("path") != "a.txt" || changed[0].Str("kind") != "update" {
		t.Fatalf("changed %v", changed)
	}
	for _, e := range ofType(ev, ToolFinished) {
		if e.Data["ok"] != true {
			t.Fatalf("tool failed: %v", e.Data)
		}
	}
	if got := ofType(ev, Usage)[0].Data["output_tokens"]; got != int64(167) {
		t.Fatalf("output_tokens %v", got)
	}
}

func TestDisplayCommandUnwrapsShells(t *testing.T) {
	ps := `"C:\\WINDOWS\\System32\\WindowsPowerShell\\v1.0\\powershell.exe" -Command `
	cases := map[string]string{
		ps + `'cat a.txt'`:              "cat a.txt",
		ps + `'echo ''hi'''`:            "echo 'hi'",
		ps + `"python -c \"print(1)\""`: `python -c "print(1)"`,
		`/bin/bash -lc 'ls -la'`:        "ls -la",
		"git status":                    "git status",
	}
	for in, want := range cases {
		if got := DisplayCommand(in); got != want {
			t.Errorf("DisplayCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCodexResumeTurn(t *testing.T) {
	if got := textOf(replay(t, NewCodexMapper(project()), "codex_resume.jsonl")); got != "goodbye" {
		t.Fatalf("text %q", got)
	}
}

func TestCodexErrorTurnReportsOnceAndKeepsWarningAsNotice(t *testing.T) {
	m := NewCodexMapper(project())
	ev := append(replay(t, m, "codex_error.jsonl"), m.Finish(1, "")...)
	if count(ev, Error) != 1 {
		t.Fatalf("errors %d", count(ev, Error))
	}
	if !strings.Contains(ofType(ev, Error)[0].Str("message"), "not supported") {
		t.Fatalf("error %v", ofType(ev, Error)[0].Data)
	}
	if count(ev, Notice) != 1 || count(ev, TurnCompleted) != 1 {
		t.Fatalf("events %v", types(ev))
	}
	if ev[len(ev)-1].Str("stop_reason") != "error" {
		t.Fatalf("last %v", ev[len(ev)-1].Data)
	}
}

// ------------------------------------------------------------------ claude

func TestClaudeToolTurn(t *testing.T) {
	m := NewClaudeMapper(project())
	ev := replay(t, m, "claude_tool.jsonl")
	if m.SessionID() != "779a99ae-8bc5-428c-9bcb-6c0c54f66d21" {
		t.Fatalf("session %q", m.SessionID())
	}
	if ev[0].Data["model"] != "claude-opus-5-5" {
		t.Fatalf("model %v", ev[0].Data["model"])
	}
	started, finished := ofType(ev, ToolStarted)[0], ofType(ev, ToolFinished)[0]
	if started.Data["id"] != finished.Data["id"] || finished.Data["ok"] != true {
		t.Fatalf("started %v finished %v", started.Data, finished.Data)
	}
	if !strings.Contains(textOf(ev), "goodbye") || ev[len(ev)-1].Type != TurnCompleted {
		t.Fatalf("events %v", types(ev))
	}
}

func TestClaudeEditToolReportsChangedFile(t *testing.T) {
	m := NewClaudeMapper(project())
	ev := m.Feed(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": "t1", "name": "Edit",
			"input": map[string]any{"file_path": filepath.Join(project(), "src", "x.py")}}}}})
	ev = append(ev, m.Feed(map[string]any{"type": "user", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok", "is_error": false}}}})...)
	changed := ofType(ev, FileChanged)
	if len(changed) != 1 || changed[0].Str("path") != filepath.Join("src", "x.py") || changed[0].Str("kind") != "update" {
		t.Fatalf("changed %v", changed)
	}
}

func TestClaudeWriteThatCreatesAFileIsAnAdd(t *testing.T) {
	m := NewClaudeMapper(project())
	target := filepath.Join(project(), "test_calc.py")
	m.Feed(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": "w1", "name": "Write", "input": map[string]any{"file_path": target}}}}})
	ev := m.Feed(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{
		"type": "tool_result", "tool_use_id": "w1", "is_error": false,
		"content": "File created successfully at: " + target}}}})
	changed := ofType(ev, FileChanged)
	if len(changed) != 1 || changed[0].Str("path") != "test_calc.py" || changed[0].Str("kind") != "add" {
		t.Fatalf("changed %v", changed)
	}
}

func TestClaudeErrorResult(t *testing.T) {
	ev := NewClaudeMapper(project()).Feed(map[string]any{"type": "result", "is_error": true, "result": "Invalid API key", "usage": map[string]any{}})
	if !slices.Equal(types(ev), []string{Usage, Error, TurnCompleted}) {
		t.Fatalf("events %v", types(ev))
	}
}

// ------------------------------------------------------------------ antigravity

func TestAntigravityReplyKeepsTheDoneTail(t *testing.T) {
	m := NewAntigravityMapper(project())
	ev := replay(t, m, "agy_pong.jsonl")
	if m.SessionID() != "027c9fb3-dc8a-458e-b380-bc6e642e094d" || ev[0].Type != SessionStarted {
		t.Fatalf("session %q first %s", m.SessionID(), ev[0].Type)
	}
	// "pong" arrives while ACTIVE and the trailing newline on DONE; both belong to the reply.
	if got := textOf(ev); got != "pong\n" {
		t.Fatalf("text %q", got)
	}
	if got := ofType(ev, Usage)[0].Data["output_tokens"]; got != int64(3134) {
		t.Fatalf("output_tokens %v", got) // already includes the 3133 thinking tokens
	}
	if ev[len(ev)-1].Str("stop_reason") != "end_turn" || count(ev, TurnCompleted) != 1 {
		t.Fatalf("events %v", types(ev))
	}
}

func TestAntigravityCommandAndEditTurn(t *testing.T) {
	m := NewAntigravityMapper(project())
	ev := replay(t, m, "agy_edit.jsonl")
	started := ofType(ev, ToolStarted)
	if len(started) != 2 || started[0].Data["name"] != "run_command" || started[1].Data["name"] != "edit_file" {
		t.Fatalf("started %v", started)
	}
	if input(started[0])["CommandLine"] != "type a.txt" {
		t.Fatalf("input %v", input(started[0]))
	}
	if !strings.Contains(ofType(ev, ToolOutput)[0].Str("chunk"), "hello") {
		t.Fatal("tool output lacks hello")
	}
	for _, e := range ofType(ev, ToolFinished) {
		if e.Data["ok"] != true {
			t.Fatalf("tool failed %v", e.Data)
		}
	}
	changed := ofType(ev, FileChanged)
	if len(changed) != 1 || changed[0].Str("path") != "a.txt" {
		t.Fatalf("changed %v", changed)
	}
	// Streamed in 13 pieces; reassembled text equals the final response exactly.
	lines := readFixtureLines(t, "agy_edit.jsonl")
	last := lines[len(lines)-1]
	if want := mapOf(last, "result")["response"]; textOf(ev) != want {
		t.Fatalf("text %q want %q", textOf(ev), want)
	}
	usage := ofType(ev, Usage)[0].Data
	if usage["input_tokens"] != int64(21827+29735) || usage["cached_input_tokens"] != int64(29735) {
		t.Fatalf("usage %v", usage)
	}
}

func TestAntigravityDeniedCommandIsExplainedNotSilent(t *testing.T) {
	ev := replay(t, NewAntigravityMapper(project()), "agy_denied.jsonl")
	finished := ofType(ev, ToolFinished)
	if len(finished) != 1 || finished[0].Data["ok"] != false { // state "ERROR" still finishes the tool
		t.Fatalf("finished %v", finished)
	}
	if !strings.Contains(ofType(ev, ToolOutput)[0].Str("chunk"), "denied") {
		t.Fatal("tool output does not say denied")
	}
	var notices []string
	for _, e := range ofType(ev, Notice) {
		notices = append(notices, e.Str("message"))
	}
	if !slices.Contains(notices, "Permission denied: Bash") {
		t.Fatalf("notices %q", notices)
	}
	if !slices.ContainsFunc(notices, func(n string) bool { return strings.Contains(n, "--permission edit") }) {
		t.Fatalf("notices %q", notices)
	}
	if textOf(ev) != "" || ev[len(ev)-1].Str("stop_reason") != "end_turn" {
		t.Fatalf("text %q events %v", textOf(ev), types(ev))
	}
}

func TestAntigravityErrorEnvelope(t *testing.T) {
	m := NewAntigravityMapper(project())
	ev := m.Feed(map[string]any{"event": "result", "result": map[string]any{
		"conversation_id": "", "status": "ERROR", "error": "invalid model selection"}})
	if m.SessionID() != "" || !slices.Equal(types(ev), []string{Usage, Error, TurnCompleted}) {
		t.Fatalf("session %q events %v", m.SessionID(), types(ev))
	}
}

func TestAntigravityCancelledEnvelope(t *testing.T) {
	ev := NewAntigravityMapper(project()).Feed(map[string]any{"event": "result", "result": map[string]any{"status": "CANCELED"}})
	if ev[len(ev)-1].Str("stop_reason") != "cancelled" {
		t.Fatalf("events %v", ev)
	}
}

func TestAntigravityWriteFileReportsAChangedFile(t *testing.T) {
	m := NewAntigravityMapper(project())
	step := func(state string) map[string]any {
		return map[string]any{"event": "step_update", "step_update": map[string]any{
			"step_index": 3.0, "step_type": "tool", "tool_name": "write_file", "state": state,
			"tool_info": map[string]any{"name": "write_file",
				"parameters": map[string]any{"Path": filepath.Join(project(), "test_calc.py")}}}}
	}
	ev := append(m.Feed(step("ACTIVE")), m.Feed(step("DONE"))...)
	changed := ofType(ev, FileChanged)
	if len(changed) != 1 || changed[0].Str("path") != "test_calc.py" {
		t.Fatalf("changed %v", changed)
	}
}

// ------------------------------------------------------------------ commands

func argAfter(argv []string, flag string) string {
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) {
		return ""
	}
	return argv[i+1]
}

func TestCodexNewAndResumeCommands(t *testing.T) {
	opts := Options{Cwd: project(), Model: "gpt-5", Permission: "read-only"}
	argv, stdin := CodexBackend{}.BuildCommand("fix & test", opts)
	if stdin == nil || *stdin != "fix & test" || slices.Contains(argv, "fix & test") {
		t.Fatalf("argv %q stdin %v", argv, stdin)
	}
	if argAfter(argv, "-s") != "read-only" || argAfter(argv, "-m") != "gpt-5" {
		t.Fatalf("argv %q", argv)
	}
	opts.SessionID = "abc"
	argv, _ = CodexBackend{}.BuildCommand("again", opts)
	if !slices.Equal(argv[:3], []string{"codex", "exec", "resume"}) || !slices.Contains(argv, "sandbox_mode=read-only") ||
		!slices.Equal(argv[len(argv)-2:], []string{"abc", "-"}) || slices.Contains(argv, "-s") {
		t.Fatalf("resume argv %q", argv)
	}
}

func TestClaudeAndAntigravityPermissions(t *testing.T) {
	// edit = auto: acceptEdits refuses every shell command headlessly, so tests could not run.
	want := map[string]string{"read-only": "plan", "edit": "auto", "full": "bypassPermissions"}
	for perm, mode := range want {
		argv, _ := ClaudeBackend{}.BuildCommand("x", Options{Cwd: project(), Permission: perm})
		if got := argAfter(argv, "--permission-mode"); got != mode {
			t.Errorf("claude %s: --permission-mode %q", perm, got)
		}
	}

	argv, stdin := AntigravityBackend{}.BuildCommand("-x & hi", Options{Cwd: project(), Permission: "read-only", SessionID: "c1"})
	if slices.Contains(argv, "-x & hi") { // prompt never on the command line
		t.Fatalf("prompt in argv %q", argv)
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(*stdin), &msg); err != nil {
		t.Fatal(err)
	}
	if msg["event"] != "user" || mapOf(msg, "message")["content"] != "-x & hi" || !strings.HasSuffix(*stdin, "\n") {
		t.Fatalf("stdin %q", *stdin)
	}
	if *stdin != `{"event": "user", "message": {"content": "-x & hi"}}`+"\n" {
		t.Fatalf("stdin not in Python's json.dumps form: %q", *stdin)
	}
	if argAfter(argv, "--input-format") != "stream-json" || argAfter(argv, "--mode") != "plan" || argAfter(argv, "--conversation") != "c1" {
		t.Fatalf("argv %q", argv)
	}
	edit, _ := AntigravityBackend{}.BuildCommand("hi", Options{Cwd: project(), Permission: "edit"})
	if !slices.Contains(edit, "--sandbox") || slices.Contains(edit, "--mode") || slices.Contains(edit, "--dangerously-skip-permissions") {
		t.Fatalf("edit argv %q", edit) // accept-edits denies every command headlessly
	}
	full, _ := AntigravityBackend{}.BuildCommand("hi", Options{Cwd: project(), Permission: "full"})
	if !slices.Contains(full, "--dangerously-skip-permissions") {
		t.Fatalf("full argv %q", full)
	}
}

func TestBadPermissionRejected(t *testing.T) {
	if _, err := NewCodex(Options{Cwd: project(), Permission: "yolo"}); err == nil {
		t.Fatal("yolo accepted")
	}
	p, err := NewClaude(Options{Cwd: project()})
	if err != nil || p.Opts.Permission != "edit" || p.ID() != ClaudeID {
		t.Fatalf("default permission: %v %+v", err, p)
	}
}

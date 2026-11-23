package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Adeel0-0/agent-orchestra/internal/players"
)

// ------------------------------------------------------------------ scripted API

// item is one scripted stream element: a chunk (JSON) or an error.
type item struct {
	chunk string
	err   error
}

func chunksOf(objs ...any) []item {
	var out []item
	for _, o := range objs {
		raw, _ := json.Marshal(o)
		out = append(out, item{chunk: string(raw)})
	}
	return out
}

func text(value string, usage map[string]any) []item {
	objs := []any{
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": value}, "finish_reason": nil}}},
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}},
	}
	if usage != nil {
		objs = append(objs, map[string]any{"choices": []any{}, "usage": usage})
	}
	return chunksOf(objs...)
}

type call struct {
	name string
	args map[string]any
}

func calls(cs ...call) []item {
	var objs []any
	for i, c := range cs {
		raw, _ := json.Marshal(c.args)
		encoded := string(raw)
		objs = append(objs,
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": i, "id": "call" + string(rune('0'+i)), "type": "function",
				"function": map[string]any{"name": c.name, "arguments": encoded[:min(5, len(encoded))]}}}}, "finish_reason": nil}}},
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": i, "function": map[string]any{"arguments": encoded[min(5, len(encoded)):]}}}}, "finish_reason": nil}}})
	}
	objs = append(objs, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}})
	return chunksOf(objs...)
}

type scripted struct {
	mu        sync.Mutex
	responses [][]item
	requests  []map[string]any
	context   int
	models    func() []map[string]any
	onRequest func(int)
}

func newScripted(responses ...[]item) *scripted {
	return &scripted{responses: responses, context: 32000}
}

func (s *scripted) Models(context.Context) ([]map[string]any, error) {
	if s.models != nil {
		return s.models(), nil
	}
	return []map[string]any{{"id": "test/tools", "context_length": json.Number(itoa(s.context)),
		"supported_parameters": []any{"tools"}, "top_provider": map[string]any{"max_completion_tokens": json.Number("8192")}}}, nil
}

func itoa(n int) string { return strconv.Itoa(n) }

func isWindowsTest() bool { return runtime.GOOS == "windows" }

func (s *scripted) Stream(ctx context.Context, body map[string]any, fn func(map[string]any) error) error {
	copied, _ := deepCopy(body)
	s.mu.Lock()
	s.requests = append(s.requests, copied.(map[string]any))
	n := len(s.requests)
	if len(s.responses) == 0 {
		s.mu.Unlock()
		return errors.New("unexpected model request")
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	hook := s.onRequest
	s.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	for _, it := range response {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if it.err != nil {
			return it.err
		}
		dec := json.NewDecoder(strings.NewReader(it.chunk))
		dec.UseNumber()
		var chunk map[string]any
		if err := dec.Decode(&chunk); err != nil {
			return err
		}
		if err := fn(chunk); err != nil {
			return err
		}
	}
	return nil
}

func (s *scripted) request(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 {
		i += len(s.requests)
	}
	return s.requests[i]
}

func (s *scripted) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func lastMessage(req map[string]any) map[string]any {
	msgs := req["messages"].([]any)
	return msgs[len(msgs)-1].(map[string]any)
}

// ------------------------------------------------------------------ helpers

func newPlayer(t *testing.T, root string, backend Backend, permission string, options ...Option) *Player {
	t.Helper()
	p, err := New(players.Options{Cwd: root, Model: "test/tools", Permission: permission},
		append([]Option{WithBackend(backend)}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type recorder struct{ events []players.Event }

func (r *recorder) emit(e players.Event) { r.events = append(r.events, e) }

func (r *recorder) of(typ string) []players.Event {
	var out []players.Event
	for _, e := range r.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (r *recorder) last() players.Event { return r.events[len(r.events)-1] }

func send(t *testing.T, p *Player, prompt string) *recorder {
	t.Helper()
	r := &recorder{}
	if err := p.Send(context.Background(), prompt, r.emit); err != nil {
		t.Fatalf("send: %v", err)
	}
	return r
}

func stopReason(e players.Event) string {
	if e.Type != players.TurnCompleted {
		return "<not completed: " + e.Type + ">"
	}
	return e.Str("stop_reason")
}

func toJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func number(v any) float64 {
	f, _ := numberValue(v, false)
	return f
}

// ------------------------------------------------------------------ tests

func TestFragmentedCallsEditThenFollowupWithUsage(t *testing.T) {
	root := t.TempDir()
	client := newScripted(
		calls(call{"write_file", map[string]any{"path": "a.txt", "content": "hello"}}),
		text("written", map[string]any{"prompt_tokens": 10, "completion_tokens": 4, "cost": 0.002}),
		text("hello", map[string]any{"prompt_tokens": 12, "completion_tokens": 2, "cost": 0.001}))
	p := newPlayer(t, root, client, "")
	r := send(t, p, "write hello")
	if b, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(b) != "hello" {
		t.Fatalf("a.txt = %q", b)
	}
	if r.events[0].Type != players.SessionStarted || stopReason(r.last()) != "end_turn" {
		t.Fatalf("events %v", r.events)
	}
	changed := r.of(players.FileChanged)
	if len(changed) != 1 || changed[0].Str("path") != "a.txt" || changed[0].Str("kind") != "add" {
		t.Fatalf("file.changed %v", changed)
	}
	result := lastMessage(client.request(1))
	if result["role"] != "tool" || result["tool_call_id"] != "call0" ||
		result["content"] != `{"ok": true, "output": "File written."}` {
		t.Fatalf("tool result %v", result)
	}
	r = send(t, p, "what did you write?")
	usage := r.of(players.Usage)[0].Data
	if number(usage["session_input_tokens"]) != 22 || math.Abs(number(usage["session_cost_usd"])-0.003) > 1e-12 {
		t.Fatalf("usage %v", usage)
	}
	if number(usage["input_tokens"]) != 12 {
		t.Fatalf("turn usage %v", usage)
	}
	if len(r.of(players.SessionStarted)) != 0 {
		t.Fatal("session.started repeated")
	}
	if !strings.Contains(toJSON(client.request(-1)["messages"]), "write hello") {
		t.Fatal("history lost")
	}
}

func TestDeniedCommandReturnsResultToModelWithoutExecution(t *testing.T) {
	root := t.TempDir()
	client := newScripted(calls(call{"run_command", map[string]any{"command": "echo unsafe > denied.txt"}}), text("done", nil))
	p := newPlayer(t, root, client, "")
	r := &recorder{}
	err := p.Send(context.Background(), "run a command", func(e players.Event) {
		r.emit(e)
		if e.Type == players.PermissionRequested {
			if e.Str("tool") != "run_command" {
				t.Errorf("tool %q", e.Str("tool"))
			}
			// Answered synchronously from inside emit, on Send's goroutine.
			if err := p.AnswerPermission(e.Str("request_id"), false); err != nil {
				t.Errorf("answer: %v", err)
			}
			if err := p.AnswerPermission(e.Str("request_id"), true); err == nil {
				t.Error("second answer accepted")
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "denied.txt")); err == nil {
		t.Fatal("denied command ran")
	}
	if !strings.Contains(strings.ToLower(lastMessage(client.request(1))["content"].(string)), "denied") {
		t.Fatal("denial not returned to the model")
	}
	if r.of(players.ToolFinished)[0].Data["ok"] != false || stopReason(r.last()) != "end_turn" {
		t.Fatalf("events %v", r.events)
	}
}

func TestApprovedCommandStreamsOutput(t *testing.T) {
	root := t.TempDir()
	client := newScripted(calls(call{"run_command", map[string]any{"command": "echo approved", "timeout": 20}}), text("done", nil))
	p := newPlayer(t, root, client, "")
	r := &recorder{}
	err := p.Send(context.Background(), "run", func(e players.Event) {
		r.emit(e)
		if e.Type == players.PermissionRequested {
			if toJSON(e.Data["input"]) != `{"command":"echo approved","timeout":20}` {
				t.Errorf("approval input %s", toJSON(e.Data["input"]))
			}
			_ = p.AnswerPermission(e.Str("request_id"), true)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, e := range r.of(players.ToolOutput) {
		out.WriteString(e.Str("chunk"))
	}
	if !strings.Contains(out.String(), "approved") || r.of(players.ToolFinished)[0].Data["ok"] != true {
		t.Fatalf("events %v", r.events)
	}
	if !strings.Contains(lastMessage(client.request(1))["content"].(string), `"ok": true`) {
		t.Fatal("result not returned")
	}
}

func TestAskModeWritePausesUntilApproved(t *testing.T) {
	root := t.TempDir()
	client := newScripted(calls(call{"write_file", map[string]any{"path": "ok.txt", "content": "yes"}}), text("done", nil))
	p := newPlayer(t, root, client, "read-only")
	err := p.Send(context.Background(), "write", func(e players.Event) {
		if e.Type == players.PermissionRequested {
			if _, err := os.Stat(filepath.Join(root, "ok.txt")); err == nil {
				t.Error("written before approval")
			}
			_ = p.AnswerPermission(e.Str("request_id"), true)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "ok.txt")); string(b) != "yes" {
		t.Fatalf("ok.txt = %q", b)
	}
}

func TestPermissionAnsweredFromAnotherGoroutine(t *testing.T) {
	root := t.TempDir()
	client := newScripted(calls(call{"write_file", map[string]any{"path": "ok.txt", "content": "yes"}}), text("done", nil))
	p := newPlayer(t, root, client, "read-only")
	ids := make(chan string, 1)
	go func() { _ = p.AnswerPermission(<-ids, true) }()
	err := p.Send(context.Background(), "write", func(e players.Event) {
		if e.Type == players.PermissionRequested {
			ids <- e.Str("request_id")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "ok.txt")); err != nil {
		t.Fatal("approved write did not run")
	}
	if err := p.AnswerPermission("unknown", true); err == nil {
		t.Fatal("unknown request accepted")
	}
}

func TestBadToolAndEscapeAreToolFailuresNotTurnCrashes(t *testing.T) {
	client := newScripted(calls(call{"write_file", map[string]any{"path": "../bad", "content": "x"}},
		call{"invented", map[string]any{}}), text("done", nil))
	r := send(t, newPlayer(t, t.TempDir(), client, ""), "try")
	failed := 0
	for _, e := range r.of(players.ToolFinished) {
		if e.Data["ok"] == false {
			failed++
		}
	}
	if failed != 2 || stopReason(r.last()) != "end_turn" {
		t.Fatalf("events %v", r.events)
	}
}

func TestMalformedToolArgumentsAreToolFailures(t *testing.T) {
	chunks := chunksOf(
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "c1", "function": map[string]any{"name": "read_file", "arguments": "[1]"}}}}}}},
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}})
	client := newScripted(chunks, text("done", nil))
	r := send(t, newPlayer(t, t.TempDir(), client, ""), "try")
	started := r.of(players.ToolStarted)[0]
	if toJSON(started.Data["input"]) != "{}" || r.of(players.ToolFinished)[0].Data["ok"] != false {
		t.Fatalf("events %v", r.events)
	}
	if !strings.Contains(lastMessage(client.request(1))["content"].(string), "must be an object") {
		t.Fatal("argument error not reported to the model")
	}
}

func TestStepLimitFinishesWithError(t *testing.T) {
	client := newScripted(calls(call{"list_dir", map[string]any{}}), calls(call{"list_dir", map[string]any{}}))
	r := send(t, newPlayer(t, t.TempDir(), client, "", WithMaxSteps(2)), "loop")
	if client.count() != 2 || stopReason(r.last()) != "error" || len(r.of(players.Error)) == 0 {
		t.Fatalf("requests %d events %v", client.count(), r.events)
	}
}

func TestStreamFailureDoesNotExecutePartialToolCall(t *testing.T) {
	root := t.TempDir()
	chunks := calls(call{"write_file", map[string]any{"path": "bad.txt", "content": "no"}})
	chunks = append(chunks[:len(chunks)-1], item{err: newError("connection lost")})
	r := send(t, newPlayer(t, root, newScripted(chunks), ""), "try")
	if _, err := os.Stat(filepath.Join(root, "bad.txt")); err == nil {
		t.Fatal("partial tool call ran")
	}
	if stopReason(r.last()) != "error" {
		t.Fatalf("events %v", r.events)
	}
}

func TestMalformedNestedStreamBecomesNormalizedError(t *testing.T) {
	for _, chunk := range []string{`{"choices":["invalid"]}`, `{"choices":[{"delta":"bad"}]}`,
		`{"choices":[{"delta":{"tool_calls":["bad"]}}]}`, `{"choices":[{"delta":{"content":5}}]}`,
		`{"usage":"x"}`, `{"choices":[{"delta":{"tool_calls":[{"index":99,"function":{}}]}}]}`} {
		r := send(t, newPlayer(t, t.TempDir(), newScripted([]item{{chunk: chunk}}), ""), "hi")
		if len(r.of(players.Error)) == 0 || stopReason(r.last()) != "error" {
			t.Errorf("%s: events %v", chunk, r.events)
		}
	}
}

func TestMissingOrDuplicateCallIdentityIsRejected(t *testing.T) {
	root := t.TempDir()
	dup := chunksOf(
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "same", "function": map[string]any{"name": "write_file", "arguments": `{"path":"x","content":"1"}`}},
			map[string]any{"index": 1, "id": "same", "function": map[string]any{"name": "write_file", "arguments": `{"path":"y","content":"1"}`}},
		}}}}},
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}})
	r := send(t, newPlayer(t, root, newScripted(dup), ""), "hi")
	if stopReason(r.last()) != "error" {
		t.Fatalf("events %v", r.events)
	}
	if _, err := os.Stat(filepath.Join(root, "x")); err == nil {
		t.Fatal("tool ran")
	}
}

func TestContextIncludesProjectInstructions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Use unittest."), 0o644); err != nil {
		t.Fatal(err)
	}
	client := newScripted(text("done", nil))
	p := newPlayer(t, root, client, "")
	send(t, p, "hi")
	system := client.request(0)["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(system, "Use unittest.") || !strings.Contains(system, p.Cwd()) {
		t.Fatalf("system %q", system)
	}
}

func TestOldTurnsSummarizedWithoutBreakingToolGroups(t *testing.T) {
	client := newScripted(text(strings.Repeat("a", 36000), nil), text("summary of older work", nil), text("next", nil))
	client.context = 12000
	p := newPlayer(t, t.TempDir(), client, "")
	send(t, p, "first")
	r := send(t, p, "second")
	if client.count() != 3 {
		t.Fatalf("%d requests", client.count())
	}
	if _, ok := client.request(1)["tools"]; ok {
		t.Fatal("summary request offered tools")
	}
	if !strings.Contains(toJSON(client.request(2)["messages"]), "summary of older work") || len(r.of(players.Notice)) == 0 {
		t.Fatalf("events %v", r.events)
	}
}

func TestCatalogOutputLimitIsUsedInsteadOf2048(t *testing.T) {
	client := newScripted(text("done", nil))
	send(t, newPlayer(t, t.TempDir(), client, ""), "write a full module")
	if number(client.request(0)["max_tokens"]) != 8192 {
		t.Fatalf("max_tokens %v", client.request(0)["max_tokens"])
	}
}

func TestSummaryAlsoRespectsSmallModelOutputCeiling(t *testing.T) {
	client := newScripted(text(strings.Repeat("a", 36000), nil), text("summary", nil), text("continued", nil))
	client.models = func() []map[string]any {
		return []map[string]any{{"id": "test/tools", "context_length": json.Number("12000"),
			"top_provider": map[string]any{"max_completion_tokens": json.Number("256")}}}
	}
	p := newPlayer(t, t.TempDir(), client, "")
	send(t, p, "first")
	send(t, p, "continue")
	if number(client.request(1)["max_tokens"]) != 256 {
		t.Fatalf("max_tokens %v", client.request(1)["max_tokens"])
	}
}

func TestLengthRecoveryDiscardsPartialToolsThenAcceptsSmallerWrite(t *testing.T) {
	root := t.TempDir()
	truncated := calls(call{"write_file", map[string]any{"path": "partial.txt", "content": "never write"}})
	truncated[len(truncated)-1] = chunksOf(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "length"}}})[0]
	client := newScripted(truncated, calls(call{"write_file", map[string]any{"path": "safe.txt", "content": "small"}}), text("done", nil))
	r := send(t, newPlayer(t, root, client, ""), "write")
	if _, err := os.Stat(filepath.Join(root, "partial.txt")); err == nil {
		t.Fatal("partial tool call ran")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "safe.txt")); string(b) != "small" {
		t.Fatalf("safe.txt = %q", b)
	}
	if stopReason(r.last()) != "end_turn" {
		t.Fatalf("events %v", r.events)
	}
	if !strings.Contains(strings.ToLower(lastMessage(client.request(1))["content"].(string)), "split") {
		t.Fatal("no continuation instruction")
	}
	for _, m := range client.request(1)["messages"].([]any) {
		if _, ok := m.(map[string]any)["tool_calls"]; ok {
			t.Fatal("truncated tool calls kept in history")
		}
	}
}

func TestLargeInstructionFilesFitFresh32kContext(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(strings.Repeat("Use ordinary project rules. ", 600)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	client := newScripted(text("done", nil))
	r := send(t, newPlayer(t, root, client, ""), "hello")
	if stopReason(r.last()) != "end_turn" || client.count() != 1 {
		t.Fatalf("events %v", r.events)
	}
}

func TestContextEstimateUsesReportedPromptTokens(t *testing.T) {
	client := newScripted(text("ok", map[string]any{"prompt_tokens": 20000}), text("summary", nil), text("continued", nil))
	client.context = 24000
	p := newPlayer(t, t.TempDir(), client, "")
	send(t, p, "start")
	r := send(t, p, "continue")
	if client.count() != 3 || len(r.of(players.Notice)) == 0 {
		t.Fatalf("requests %d events %v", client.count(), r.events)
	}
}

func TestPromptMeasurementSurvivesPlayerEvictionAndRestore(t *testing.T) {
	first := newScripted(text("ok", map[string]any{"prompt_tokens": 20000}))
	first.context = 24000
	original := newPlayer(t, t.TempDir(), first, "")
	send(t, original, "start")
	resumedClient := newScripted(text("summary", nil), text("continued", nil))
	resumedClient.context = 24000
	resumed := newPlayer(t, t.TempDir(), resumedClient, "")
	if err := resumed.RestoreState(original.SnapshotState()); err != nil {
		t.Fatal(err)
	}
	r := send(t, resumed, "continue")
	if resumedClient.count() != 2 || len(r.of(players.Notice)) == 0 {
		t.Fatalf("requests %d events %v", resumedClient.count(), r.events)
	}
}

func TestCancelPendingPermissionPreservesValidFollowupHistory(t *testing.T) {
	client := newScripted(calls(call{"run_command", map[string]any{"command": "echo x"}}), text("done", nil))
	p := newPlayer(t, t.TempDir(), client, "")
	ctx, cancel := context.WithCancel(context.Background())
	r := &recorder{}
	err := p.Send(ctx, "do it", func(e players.Event) {
		r.emit(e)
		if e.Type == players.PermissionRequested {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(r.of(players.TurnCompleted)) != 0 {
		t.Fatal("cancelled turn emitted turn.completed (contract: return ctx.Err())")
	}
	send(t, p, "continue")
	var results []map[string]any
	for _, m := range client.request(-1)["messages"].([]any) {
		if m.(map[string]any)["role"] == "tool" {
			results = append(results, m.(map[string]any))
		}
	}
	if len(results) != 1 || !strings.Contains(strings.ToLower(results[0]["content"].(string)), "cancel") {
		t.Fatalf("tool results %v", results)
	}
}

func TestCancelDuringCommandKillsItAndRecordsCancellation(t *testing.T) {
	root := t.TempDir()
	command := "sleep 30"
	if isWindowsTest() {
		command = "Start-Sleep -Seconds 30"
	}
	client := newScripted(calls(call{"run_command", map[string]any{"command": command}}), text("done", nil))
	p := newPlayer(t, root, client, "full")
	ctx, cancel := context.WithCancel(context.Background())
	err := p.Send(ctx, "sleep", func(e players.Event) {
		if e.Type == players.ToolStarted {
			go cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	send(t, p, "continue")
	if !strings.Contains(toJSON(client.request(-1)["messages"]), "Tool cancelled before completion.") {
		t.Fatal("cancelled tool result missing")
	}
}

func TestConcurrentSendIsRefused(t *testing.T) {
	client := newScripted(calls(call{"run_command", map[string]any{"command": "echo x"}}), text("done", nil))
	p := newPlayer(t, t.TempDir(), client, "")
	err := p.Send(context.Background(), "first", func(e players.Event) {
		if e.Type == players.PermissionRequested {
			inner := &recorder{}
			_ = p.Send(context.Background(), "second", inner.emit)
			if len(inner.events) != 2 || stopReason(inner.last()) != "error" {
				t.Errorf("inner events %v", inner.events)
			}
			_ = p.AnswerPermission(e.Str("request_id"), false)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResumeIsExplicitlyUnsupportedUntilSessionStorage(t *testing.T) {
	_, err := New(players.Options{Cwd: t.TempDir(), Model: "test/tools", SessionID: "old"}, WithBackend(newScripted()))
	if err == nil || !strings.Contains(err.Error(), "resume") {
		t.Fatalf("err = %v", err)
	}
	if _, err := New(players.Options{Cwd: t.TempDir()}, WithBackend(newScripted())); err == nil || !strings.Contains(err.Error(), "--model") {
		t.Fatalf("missing model err = %v", err)
	}
	if _, err := New(players.Options{Cwd: t.TempDir(), Model: "m", Permission: "bad"}); err == nil {
		t.Fatal("bad permission accepted")
	}
	if _, err := New(players.Options{Cwd: t.TempDir(), Model: "m"}, WithMaxSteps(51)); err == nil {
		t.Fatal("max steps 51 accepted")
	}
}

func TestPersistedSnapshotRestoresHistoryUsageWithoutCredentials(t *testing.T) {
	client := newScripted(text("remembered", map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "cost": 0.001}))
	original := newPlayer(t, t.TempDir(), client, "")
	rev := original.SnapshotRevision()
	send(t, original, "remember this")
	if original.SnapshotRevision() == rev {
		t.Fatal("revision unchanged after a turn")
	}
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(toJSON(original.SnapshotState())), &snapshot); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for k := range snapshot {
		keys = append(keys, k)
	}
	if len(keys) != 4 || snapshot["history"] == nil || snapshot["prompt_measurement"] == nil {
		t.Fatalf("snapshot keys %v", keys)
	}
	if strings.Contains(toJSON(snapshot), "key") && strings.Contains(toJSON(snapshot), "Bearer") {
		t.Fatal("credentials in snapshot")
	}
	resumedClient := newScripted(text("continued", nil))
	resumed := newPlayer(t, t.TempDir(), resumedClient, "")
	if err := resumed.RestoreState(snapshot); err != nil { // float64 numbers, as a plain decoder gives
		t.Fatal(err)
	}
	r := send(t, resumed, "continue")
	if !strings.Contains(toJSON(resumedClient.request(0)), "remember this") {
		t.Fatal("history not restored")
	}
	if number(r.of(players.Usage)[0].Data["session_input_tokens"]) != 10 {
		t.Fatalf("usage %v", r.of(players.Usage)[0].Data)
	}
}

func TestRestoreRepairsUnansweredToolsWithoutReexecuting(t *testing.T) {
	root := t.TempDir()
	client := newScripted(text("done", nil))
	p := newPlayer(t, root, client, "")
	err := p.RestoreState(map[string]any{
		"history": []any{
			map[string]any{"role": "user", "content": "do something"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "old", "type": "function",
				"function": map[string]any{"name": "write_file", "arguments": `{"path":"danger","content":"no"}`}}}},
		},
		"totals":         map[string]any{"input_tokens": 0, "output_tokens": 0, "cost_usd": 0},
		"context_length": 32000,
	})
	if err != nil {
		t.Fatal(err)
	}
	send(t, p, "continue")
	var results []string
	for _, m := range client.request(0)["messages"].([]any) {
		if m.(map[string]any)["role"] == "tool" {
			results = append(results, m.(map[string]any)["content"].(string))
		}
	}
	if len(results) != 1 || !strings.Contains(strings.ToLower(results[0]), "interrupted") {
		t.Fatalf("results %v", results)
	}
	if _, err := os.Stat(filepath.Join(root, "danger")); err == nil {
		t.Fatal("interrupted tool re-executed")
	}
}

func TestRestoreRejectsInvalidState(t *testing.T) {
	p := newPlayer(t, t.TempDir(), newScripted(), "")
	good := func() map[string]any {
		return map[string]any{"history": []any{}, "totals": map[string]any{"input_tokens": 0, "output_tokens": 0, "cost_usd": 0},
			"context_length": nil}
	}
	cases := []func(map[string]any){
		func(s map[string]any) { delete(s, "totals") },
		func(s map[string]any) { s["extra"] = 1 },
		func(s map[string]any) { s["history"] = []any{map[string]any{"role": "system"}} },
		func(s map[string]any) {
			s["totals"] = map[string]any{"input_tokens": -1, "output_tokens": 0, "cost_usd": 0}
		},
		func(s map[string]any) { s["totals"] = map[string]any{"input_tokens": 0, "output_tokens": 0} },
		func(s map[string]any) { s["context_length"] = 0 },
		func(s map[string]any) { s["prompt_measurement"] = map[string]any{"tokens": 1.5, "bytes": 0} },
		func(s map[string]any) {
			s["history"] = []any{map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": 5}}}}
		},
	}
	for i, mutate := range cases {
		s := good()
		mutate(s)
		if err := p.RestoreState(s); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if err := p.RestoreState(good()); err != nil {
		t.Fatalf("good state: %v", err)
	}
}

func TestReasoningDetailsAreMergedIntoHistory(t *testing.T) {
	chunks := chunksOf(
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"reasoning": "think", "reasoning_details": []any{
			map[string]any{"index": 0, "type": "reasoning.text", "text": "a"}}}}}},
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "hi", "reasoning_details": []any{
			map[string]any{"index": 0, "type": "reasoning.text", "text": "b", "signature": "s"}}}, "finish_reason": "stop"}}})
	p := newPlayer(t, t.TempDir(), newScripted(chunks), "")
	r := send(t, p, "hi")
	if len(r.of(players.ReasoningDelta)) != 1 || len(r.of(players.TextDelta)) != 1 {
		t.Fatalf("events %v", r.events)
	}
	history := p.SnapshotState()["history"].([]any)
	assistant := history[1].(map[string]any)
	details := assistant["reasoning_details"].([]any)
	if len(details) != 1 || details[0].(map[string]any)["text"] != "ab" || details[0].(map[string]any)["signature"] != "s" {
		t.Fatalf("assistant %v", assistant)
	}
	if _, ok := assistant["reasoning"]; ok {
		t.Fatal("reasoning kept alongside reasoning_details")
	}
}

func TestMissingModelInCatalogIsTurnError(t *testing.T) {
	client := newScripted()
	client.models = func() []map[string]any { return nil }
	r := send(t, newPlayer(t, t.TempDir(), client, ""), "hi")
	if stopReason(r.last()) != "error" || !strings.Contains(r.of(players.Error)[0].Str("message"), "orchestra models") {
		t.Fatalf("events %v", r.events)
	}
}

func TestPyDumpsMatchesPythonJSON(t *testing.T) {
	v := map[string]any{"content": "é\n\"\x01\u007f", "role": "user", "n": json.Number("3"), "z": []any{true, nil, 1.5}}
	if got := pyDumps(v, false); got != "{\"role\": \"user\", \"content\": \"é\\n\\\"\\u0001\u007f\", \"n\": 3, \"z\": [true, null, 1.5]}" {
		t.Fatalf("ensure_ascii=False: %s", got)
	}
	if got := pyDumps("é😀\u007f", true); got != `"\u00e9\ud83d\ude00\u007f"` {
		t.Fatalf("ensure_ascii=True: %s", got)
	}
}

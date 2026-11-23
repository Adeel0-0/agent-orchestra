// Package players is the contract every agent backend follows (orchestra/players/base.py).
//
// A player wraps one backend. Whatever the backend speaks, it emits the same small
// set of events, so the terminal chat, the app server and the transcript store never
// need to know which model is behind a session.
//
// CLI players are split in two: a Mapper turns one parsed JSON line from the CLI into
// zero or more events and does no I/O (so it is tested against recorded fixtures),
// and CLIPlayer runs the CLI as a streaming process and feeds every line to a Mapper.
package players

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Adeel0-0/agent-orchestra/internal/runner"
)

// Event types. The data fields each type carries are listed beside it.
const (
	SessionStarted      = "session.started"      // backend_session_id, model
	TextDelta           = "text.delta"           // text
	ReasoningDelta      = "reasoning.delta"      // text
	ToolStarted         = "tool.started"         // id, name, input
	ToolOutput          = "tool.output"          // id, chunk
	ToolFinished        = "tool.finished"        // id, ok
	PermissionRequested = "permission.requested" // request_id, tool, input, reason
	FileChanged         = "file.changed"         // path, kind
	Usage               = "usage"                // input_tokens, output_tokens, cost_usd
	Notice              = "notice"               // message (warnings that do not stop the turn)
	Error               = "error"                // message, kind, retryable
	TurnCompleted       = "turn.completed"       // stop_reason: end_turn | error | cancelled
)

// PermissionModes are the accepted --permission values.
var PermissionModes = []string{"read-only", "edit", "full"}

// ValidPermission reports whether p is one of PermissionModes.
func ValidPermission(p string) bool {
	for _, m := range PermissionModes {
		if m == p {
			return true
		}
	}
	return false
}

// Event is one normalized player event. It serializes flat, like Python's
// Event.to_dict(): {"type": ..., <data fields>}.
type Event struct {
	Type string
	Data map[string]any
}

// NewEvent builds an event; data may be nil.
func NewEvent(typ string, data map[string]any) Event {
	if data == nil {
		data = map[string]any{}
	}
	return Event{Type: typ, Data: data}
}

// ToMap is the flat form stored in transcripts and sent to the UI.
func (e Event) ToMap() map[string]any {
	m := make(map[string]any, len(e.Data)+1)
	for k, v := range e.Data {
		m[k] = v
	}
	m["type"] = e.Type
	return m
}

// MarshalJSON writes the flat form.
func (e Event) MarshalJSON() ([]byte, error) { return json.Marshal(e.ToMap()) }

// Str returns a string field, or "" if absent or not a string.
func (e Event) Str(key string) string {
	s, _ := e.Data[key].(string)
	return s
}

// Options construct a player for one session.
type Options struct {
	Cwd        string
	Model      string // "" means the backend's default
	Permission string // one of PermissionModes; "" means "edit"
	SessionID  string // backend session to resume; "" starts a new one
}

// Player is one agent backend for one session.
//
// Send runs one turn, calling emit for every event in order, and returns when the turn
// has ended. It always emits exactly one TurnCompleted (even on failure) unless ctx is
// cancelled, in which case it stops the backend (killing any process tree) and returns
// ctx.Err(). emit is called from Send's goroutine only.
type Player interface {
	ID() string
	SessionID() string
	Send(ctx context.Context, prompt string, emit func(Event)) error
	// AnswerPermission resolves a pending PermissionRequested. CLI players return an error.
	AnswerPermission(requestID string, allow bool) error
}

// Snapshotter is implemented by players whose conversation lives in Orchestra
// (OpenRouter), so the server can save and restore it across restarts.
type Snapshotter interface {
	SnapshotState() map[string]any
	SnapshotRevision() int64
	RestoreState(state map[string]any) error
}

// ClassifyError sorts a backend error into a kind the UI and the router can act on.
//
// Returns (kind, retryable). Kinds the rest of Orchestra understands:
//
//	"auth"  - not logged in or login expired. Show the login command. Not retryable.
//	"quota" - subscription or rate limit reached. Retryable later, or on another player.
//	"model" - unknown or unsupported model. Not retryable with the same settings.
//	"other" - anything else.
//
// TODO(owner): decide how messages map to kinds (kept from the Python TODO in
// orchestra/players/base.py). This drives what the user is told and, in Phase 6, when
// Orchestra falls over to a different player. Real messages seen so far:
//
//	Codex: "The 'no-such-model-xyz' model is not supported when using Codex with a ChatGPT account."
//	agy:   "authentication required" (documented), "invalid model selection ..." (documented)
func ClassifyError(message string) (kind string, retryable bool) {
	return "other", false
}

// ErrorEvent builds an Error event with its classification.
func ErrorEvent(message string) Event {
	kind, retryable := ClassifyError(message)
	return NewEvent(Error, map[string]any{"message": message, "kind": kind, "retryable": retryable})
}

// ------------------------------------------------------------------ mapper

// Mapper turns one CLI's parsed JSON lines into events.
type Mapper interface {
	Feed(obj map[string]any) []Event
	Finish(exitCode int, stderr string) []Event
	SessionID() string
}

// MapperBase holds the state and helpers every CLI mapper shares. Embed it and
// implement Feed.
type MapperBase struct {
	Cwd         string
	Session     string
	Completed   bool
	Errored     bool
	textStarted bool
}

// NewMapperBase returns a base rooted at cwd.
func NewMapperBase(cwd string) MapperBase {
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	return MapperBase{Cwd: cwd}
}

// SessionID is the backend session id seen so far.
func (m *MapperBase) SessionID() string { return m.Session }

// TextStarted reports whether any assistant text was emitted this turn.
func (m *MapperBase) TextStarted() bool { return m.textStarted }

// Text emits assistant text. newBlock separates whole messages with a blank line.
func (m *MapperBase) Text(text string, newBlock bool) []Event {
	if text == "" {
		return nil
	}
	if newBlock && m.textStarted {
		text = "\n\n" + text
	}
	m.textStarted = true
	return []Event{NewEvent(TextDelta, map[string]any{"text": text})}
}

// Err emits an error and remembers that the turn failed.
func (m *MapperBase) Err(message string) []Event {
	m.Errored = true
	return []Event{ErrorEvent(message)}
}

// Complete emits TurnCompleted once.
func (m *MapperBase) Complete(stopReason string) []Event {
	if m.Completed {
		return nil
	}
	m.Completed = true
	return []Event{NewEvent(TurnCompleted, map[string]any{"stop_reason": stopReason})}
}

// RelPath shows paths inside the project relative to it.
func (m *MapperBase) RelPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(m.Cwd, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}

// Finish guarantees every turn ends with TurnCompleted, reporting a failed exit.
func (m *MapperBase) Finish(exitCode int, stderr string) []Event {
	var events []Event
	if exitCode != 0 && !m.Errored {
		lines := strings.Split(strings.TrimSpace(stderr), "\n")
		if len(lines) > 5 {
			lines = lines[len(lines)-5:]
		}
		tail := strings.TrimSpace(strings.Join(lines, "\n"))
		if tail == "" {
			tail = fmt.Sprintf("exited with code %d", exitCode)
		}
		events = append(events, m.Err(tail)...)
	}
	if !m.Completed {
		reason := "end_turn"
		if exitCode != 0 || m.Errored {
			reason = "error"
		}
		events = append(events, m.Complete(reason)...)
	}
	return events
}

// ------------------------------------------------------------------ CLI player

// CLIBackend is what a CLI player provides: how to run one turn and how to read it.
type CLIBackend interface {
	// BuildCommand returns argv and the text for stdin (nil for none). Prompts go on
	// stdin whenever the CLI allows it: npm CLIs are cmd.exe shims on Windows.
	BuildCommand(prompt string, opts Options) (argv []string, stdin *string)
	NewMapper(cwd string) Mapper
}

// safeSessionID matches backend session ids that are safe to put in argv: they come
// from the CLI's own output and are stored, so they are checked before reuse (an id
// starting with "-" would be read as an option).
var safeSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// ValidSessionID reports whether id may be passed to a CLI to resume a session.
func ValidSessionID(id string) bool { return safeSessionID.MatchString(id) }

// CLIPlayer runs a CLIBackend as a streaming process.
type CLIPlayer struct {
	PlayerID string
	Backend  CLIBackend
	Opts     Options
}

// NewCLIPlayer validates options and resolves the folder.
func NewCLIPlayer(id string, backend CLIBackend, opts Options) (*CLIPlayer, error) {
	if opts.Permission == "" {
		opts.Permission = "edit"
	}
	if !ValidPermission(opts.Permission) {
		return nil, fmt.Errorf("permission must be one of %v", PermissionModes)
	}
	if opts.SessionID != "" && !ValidSessionID(opts.SessionID) {
		return nil, fmt.Errorf("refusing to resume: saved session id %q is not a plain id", opts.SessionID)
	}
	abs, err := filepath.Abs(opts.Cwd)
	if err != nil {
		return nil, err
	}
	opts.Cwd = abs
	return &CLIPlayer{PlayerID: id, Backend: backend, Opts: opts}, nil
}

// ID is the registry id ("codex", "claude", "antigravity").
func (p *CLIPlayer) ID() string { return p.PlayerID }

// SessionID is the backend session to resume next turn.
func (p *CLIPlayer) SessionID() string { return p.Opts.SessionID }

// AnswerPermission is unsupported: these CLIs cannot pause for Orchestra approvals headlessly.
func (p *CLIPlayer) AnswerPermission(string, bool) error {
	return errors.New("this CLI backend does not support live permission requests")
}

// Send runs one turn. Cancelling ctx kills the CLI and everything it started.
func (p *CLIPlayer) Send(ctx context.Context, prompt string, emit func(Event)) error {
	argv, stdin := p.Backend.BuildCommand(prompt, p.Opts)
	mapper := p.Backend.NewMapper(p.Opts.Cwd)
	proc, err := runner.StartStream(argv, runner.StreamOptions{Cwd: p.Opts.Cwd, Stdin: stdin})
	if err != nil {
		events := mapper.Finish(-1, err.Error())
		for _, e := range events {
			emit(e)
		}
		return nil
	}

	stop := make(chan struct{})
	killed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			proc.Kill()
			close(killed)
		case <-stop:
		}
	}()
	defer close(stop)

	_ = proc.Lines(func(line string) bool {
		if ctx.Err() != nil {
			return false
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil || obj == nil {
			return true // CLIs occasionally print plain text; it is not an event
		}
		for _, e := range mapper.Feed(obj) {
			if e.Type == SessionStarted {
				if id := e.Str("backend_session_id"); id != "" && ValidSessionID(id) {
					p.Opts.SessionID = id
				}
			}
			emit(e)
		}
		return true
	})
	if ctx.Err() != nil {
		<-killed
		return ctx.Err()
	}
	code := proc.Wait()
	for _, e := range mapper.Finish(code, proc.Stderr()) {
		emit(e)
	}
	proc.Kill() // reap anything the CLI left in its group
	return nil
}

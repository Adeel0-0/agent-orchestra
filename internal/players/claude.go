package players

// Claude Pro / Max through the official Claude Code CLI
// (`claude -p --output-format stream-json`) (orchestra/players/claude.py).
//
// Event shapes were recorded from Claude Code 2.1.319 (testdata/claude_tool.jsonl):
//
//	system/init {session_id, model}
//	assistant {message.content[]: text | thinking | tool_use}
//	user {message.content[]: tool_result}
//	result {is_error, result, stop_reason, total_cost_usd, usage, permission_denials}

import (
	"strings"
	"unicode"
)

// Claude Code identity and hints (ClaudePlayer's class attributes).
const (
	ClaudeID          = "claude"
	ClaudeDisplayName = "Claude Code"
	ClaudeExecutable  = "claude"
	ClaudeInstallHint = "npm install -g @anthropic-ai/claude-code"
	ClaudeLoginHint   = "claude  (then /login)"
)

// ClaudeLoginCheck prints JSON with loggedIn.
var ClaudeLoginCheck = []string{"claude", "auth", "status"}

// "edit" is Claude Code's auto mode: edits and commands run without prompting, and Claude
// Code's own safety checker blocks risky actions. acceptEdits was tried first, but headless
// it refuses every shell command (tests could not run), and Claude Code's sandbox is not
// active on Windows, so there is no sandboxed alternative. Owner decision, 2026-11-21.
var claudePermission = map[string]string{"read-only": "plan", "edit": "auto", "full": "bypassPermissions"}

var claudeEditTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

func claudeResultText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, item := range c {
			if m, ok := item.(map[string]any); ok {
				b.WriteString(strOf(m, "text", ""))
			}
		}
		return b.String()
	}
	return ""
}

// ClaudeMapper maps Claude Code stream-json lines.
type ClaudeMapper struct {
	MapperBase
	edits map[string]string // tool_use id -> path, for file.changed on success
}

// NewClaudeMapper returns a mapper rooted at cwd.
func NewClaudeMapper(cwd string) *ClaudeMapper {
	return &ClaudeMapper{MapperBase: NewMapperBase(cwd), edits: map[string]string{}}
}

func contentBlocks(obj map[string]any) []any {
	return listOf(mapOf(obj, "message"), "content")
}

// Feed maps one JSON line.
func (m *ClaudeMapper) Feed(obj map[string]any) []Event {
	kind, _ := obj["type"].(string)
	switch {
	case kind == "system" && obj["subtype"] == "init":
		m.Session, _ = obj["session_id"].(string)
		return []Event{NewEvent(SessionStarted, map[string]any{"backend_session_id": orNil(m.Session), "model": obj["model"]})}
	case kind == "assistant":
		return m.assistant(obj)
	case kind == "user":
		return m.user(obj)
	case kind == "result":
		return m.result(obj)
	}
	return nil
}

func (m *ClaudeMapper) assistant(obj map[string]any) []Event {
	var events []Event
	for _, b := range contentBlocks(obj) {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			events = append(events, m.Text(strOf(block, "text", ""), true)...)
		case "thinking":
			events = append(events, NewEvent(ReasoningDelta, map[string]any{"text": strOf(block, "thinking", "")}))
		case "tool_use":
			var input any = map[string]any{}
			if truthy(block["input"]) {
				input = block["input"]
			}
			id, name := strOf(block, "id", ""), strOf(block, "name", "")
			if claudeEditTools[name] {
				if in, ok := input.(map[string]any); ok {
					path := strOf(in, "file_path", "")
					if path == "" {
						path = strOf(in, "notebook_path", "")
					}
					if path != "" {
						m.edits[id] = path
					}
				}
			}
			events = append(events, NewEvent(ToolStarted, map[string]any{"id": id, "name": name, "input": input}))
		}
	}
	return events
}

func (m *ClaudeMapper) user(obj map[string]any) []Event {
	var events []Event
	for _, b := range contentBlocks(obj) {
		block, ok := b.(map[string]any)
		if !ok || block["type"] != "tool_result" {
			continue
		}
		tid := strOf(block, "tool_use_id", "")
		success := !truthy(block["is_error"])
		output := claudeResultText(block["content"])
		if output != "" {
			events = append(events, NewEvent(ToolOutput, map[string]any{"id": tid, "chunk": output}))
		}
		if path, edited := m.edits[tid]; success && edited {
			delete(m.edits, tid)
			// Claude's Write reports "File created successfully at: ..." for a new file.
			kind := "update"
			if strings.HasPrefix(strings.ToLower(strings.TrimLeftFunc(output, unicode.IsSpace)), "file created") {
				kind = "add"
			}
			events = append(events, NewEvent(FileChanged, map[string]any{"path": m.RelPath(path), "kind": kind}))
		}
		events = append(events, NewEvent(ToolFinished, map[string]any{"id": tid, "ok": success}))
	}
	return events
}

func (m *ClaudeMapper) result(obj map[string]any) []Event {
	u := mapOf(obj, "usage")
	events := []Event{NewEvent(Usage, map[string]any{
		"input_tokens": intOf(u, "input_tokens") + intOf(u, "cache_creation_input_tokens") +
			intOf(u, "cache_read_input_tokens"),
		"cached_input_tokens": intOf(u, "cache_read_input_tokens"),
		"output_tokens":       intOf(u, "output_tokens"),
		// What the run would cost at API prices; a subscription login does not bill it.
		"cost_usd": obj["total_cost_usd"],
	})}
	for _, d := range listOf(obj, "permission_denials") {
		denial, _ := d.(map[string]any)
		events = append(events, NewEvent(Notice, map[string]any{"message": "Permission denied: " + strOf(denial, "tool_name", "tool")}))
	}
	if truthy(obj["is_error"]) {
		msg := "Claude reported an error"
		if truthy(obj["result"]) {
			msg = PyStr(obj["result"])
		} else if truthy(obj["subtype"]) {
			msg = PyStr(obj["subtype"])
		}
		events = append(events, m.Err(msg)...)
		return append(events, m.Complete("error")...)
	}
	reason := "end_turn"
	if truthy(obj["stop_reason"]) {
		reason = PyStr(obj["stop_reason"])
	}
	return append(events, m.Complete(reason)...)
}

// ClaudeBackend builds `claude -p` commands.
type ClaudeBackend struct{}

// BuildCommand returns argv and the prompt for stdin.
func (ClaudeBackend) BuildCommand(prompt string, opts Options) ([]string, *string) {
	argv := []string{"claude", "-p", "--output-format", "stream-json", "--verbose",
		"--permission-mode", claudePermission[permissionOrEdit(opts.Permission)]}
	if opts.Model != "" {
		argv = append(argv, "--model", opts.Model)
	}
	if opts.SessionID != "" {
		argv = append(argv, "--resume", opts.SessionID)
	}
	return argv, &prompt
}

// NewMapper returns a Claude mapper.
func (ClaudeBackend) NewMapper(cwd string) Mapper { return NewClaudeMapper(cwd) }

// NewClaude returns a Claude Code player.
func NewClaude(opts Options) (*CLIPlayer, error) {
	return NewCLIPlayer(ClaudeID, ClaudeBackend{}, opts)
}

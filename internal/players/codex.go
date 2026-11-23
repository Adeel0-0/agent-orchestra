package players

// ChatGPT Plus / Pro through the official Codex CLI (`codex exec --json`)
// (orchestra/players/codex.py).
//
// Event shapes were recorded from codex-cli 0.166.0 (tests/fixtures/codex_*.jsonl):
//
//	thread.started {thread_id}            turn.started
//	item.started / item.completed {item}  item.type: agent_message, reasoning,
//	                                      command_execution, file_change, error, ...
//	turn.completed {usage}                error {message}   turn.failed {error}

import (
	"regexp"
	"strings"
)

// Codex identity and hints (CodexPlayer's class attributes).
const (
	CodexID          = "codex"
	CodexDisplayName = "Codex (ChatGPT)"
	CodexExecutable  = "codex"
	CodexInstallHint = "npm install -g @openai/codex"
	CodexLoginHint   = "codex login"
)

// CodexLoginCheck is the command whose exit code 0 means logged in.
var CodexLoginCheck = []string{"codex", "login", "status"}

var codexSandbox = map[string]string{"read-only": "read-only", "edit": "workspace-write", "full": "danger-full-access"}

// Codex runs every command through a shell wrapper; show the command the model actually
// wrote. Python uses one regex with a backreference for the quote; RE2 has none, so
// there is one pattern per quote character, tried in the same order.
var codexWrappers = []struct {
	re    *regexp.Regexp
	quote string
}{
	{regexp.MustCompile(`(?is)^\s*"?[^"]*?(?:powershell|pwsh)(?:\.exe)?"?\s+(?:-NoProfile\s+)?-Command\s+'(.*)'\s*$`), "'"},
	{regexp.MustCompile(`(?is)^\s*"?[^"]*?(?:powershell|pwsh)(?:\.exe)?"?\s+(?:-NoProfile\s+)?-Command\s+"(.*)"\s*$`), `"`},
	{regexp.MustCompile(`(?is)^\s*"?[^"]*?(?:bash|sh|zsh)"?\s+-l?c\s+'(.*)'\s*$`), "'"},
	{regexp.MustCompile(`(?is)^\s*"?[^"]*?(?:bash|sh|zsh)"?\s+-l?c\s+"(.*)"\s*$`), `"`},
}

// DisplayCommand unwraps Codex's PowerShell or bash wrapper.
func DisplayCommand(command string) string {
	for _, w := range codexWrappers {
		m := w.re.FindStringSubmatch(command)
		if m == nil {
			continue
		}
		inner := m[1]
		if w.quote == "'" {
			// PowerShell doubles a quote to escape it inside a same-quoted string.
			return strings.ReplaceAll(inner, "''", "'")
		}
		return strings.ReplaceAll(inner, `\"`, `"`)
	}
	return command
}

// CodexMapper maps `codex exec --json` lines.
type CodexMapper struct {
	MapperBase
}

// NewCodexMapper returns a mapper rooted at cwd.
func NewCodexMapper(cwd string) *CodexMapper {
	return &CodexMapper{MapperBase: NewMapperBase(cwd)}
}

// Feed maps one JSON line.
func (m *CodexMapper) Feed(obj map[string]any) []Event {
	switch kind, _ := obj["type"].(string); kind {
	case "thread.started":
		m.Session, _ = obj["thread_id"].(string)
		return []Event{NewEvent(SessionStarted, map[string]any{"backend_session_id": orNil(m.Session), "model": nil})}
	case "item.started", "item.completed":
		return m.item(mapOf(obj, "item"), kind == "item.completed")
	case "turn.completed":
		u := mapOf(obj, "usage")
		events := []Event{NewEvent(Usage, map[string]any{
			"input_tokens":        intOf(u, "input_tokens"),
			"cached_input_tokens": intOf(u, "cached_input_tokens"),
			"output_tokens":       intOf(u, "output_tokens"),
			"cost_usd":            nil, // covered by the subscription
		})}
		return append(events, m.Complete("end_turn")...)
	case "error":
		return m.Err(strOf(obj, "message", "Codex reported an error"))
	case "turn.failed":
		var events []Event
		if !m.Errored {
			events = m.Err(strOf(mapOf(obj, "error"), "message", "Turn failed"))
		}
		return append(events, m.Complete("error")...)
	}
	return nil
}

func (m *CodexMapper) item(item map[string]any, done bool) []Event {
	itype, _ := item["type"].(string)
	iid := strOf(item, "id", "")

	switch itype {
	case "agent_message":
		if !done {
			return nil
		}
		return m.Text(strOf(item, "text", ""), true)

	case "reasoning":
		if !done {
			return nil
		}
		return []Event{NewEvent(ReasoningDelta, map[string]any{"text": strOf(item, "text", "")})}

	case "command_execution":
		if !done {
			raw := strOf(item, "command", "")
			return []Event{NewEvent(ToolStarted, map[string]any{"id": iid, "name": "shell",
				"input": map[string]any{"command": DisplayCommand(raw), "raw_command": raw}})}
		}
		var events []Event
		if truthy(item["aggregated_output"]) {
			events = append(events, NewEvent(ToolOutput, map[string]any{"id": iid, "chunk": strOf(item, "aggregated_output", "")}))
		}
		code, isNum := item["exit_code"].(float64)
		return append(events, NewEvent(ToolFinished, map[string]any{"id": iid, "ok": isNum && code == 0}))

	case "file_change":
		changes := listOf(item, "changes")
		if !done {
			paths := make([]any, 0, len(changes))
			for _, c := range changes {
				cm, _ := c.(map[string]any)
				paths = append(paths, m.RelPath(strOf(cm, "path", "")))
			}
			return []Event{NewEvent(ToolStarted, map[string]any{"id": iid, "name": "edit", "input": map[string]any{"paths": paths}})}
		}
		ok := strOf(item, "status", "") == "completed"
		var events []Event
		if ok {
			for _, c := range changes {
				cm, _ := c.(map[string]any)
				events = append(events, NewEvent(FileChanged, map[string]any{
					"path": m.RelPath(strOf(cm, "path", "")), "kind": strOf(cm, "kind", "update"),
				}))
			}
		}
		return append(events, NewEvent(ToolFinished, map[string]any{"id": iid, "ok": ok}))

	case "error":
		// Item-level errors are warnings (e.g. "Model metadata ... not found"); the turn goes on.
		if !done {
			return nil
		}
		return []Event{NewEvent(Notice, map[string]any{"message": strOf(item, "message", "")})}
	}

	// mcp_tool_call, web_search, todo_list and anything newer: show it generically.
	if !done {
		name := itype
		if name == "" {
			name = "tool"
		}
		return []Event{NewEvent(ToolStarted, map[string]any{"id": iid, "name": name, "input": map[string]any{}})}
	}
	return []Event{NewEvent(ToolFinished, map[string]any{"id": iid, "ok": strOf(item, "status", "") != "failed"})}
}

// CodexBackend builds `codex exec` commands.
type CodexBackend struct{}

// BuildCommand returns argv and the prompt for stdin.
func (CodexBackend) BuildCommand(prompt string, opts Options) ([]string, *string) {
	sandbox := codexSandbox[permissionOrEdit(opts.Permission)]
	common := []string{"--json", "--skip-git-repo-check"}
	if opts.Model != "" {
		common = append(common, "-m", opts.Model)
	}
	var argv []string
	if opts.SessionID != "" {
		// `exec resume` has no -s or -C: the sandbox goes through config, the folder is our cwd.
		argv = append([]string{"codex", "exec", "resume"}, common...)
		argv = append(argv, "-c", "sandbox_mode="+sandbox, opts.SessionID, "-")
	} else {
		argv = append([]string{"codex", "exec"}, common...)
		argv = append(argv, "-s", sandbox, "-")
	}
	return argv, &prompt
}

// NewMapper returns a Codex mapper.
func (CodexBackend) NewMapper(cwd string) Mapper { return NewCodexMapper(cwd) }

// NewCodex returns a Codex player.
func NewCodex(opts Options) (*CLIPlayer, error) { return NewCLIPlayer(CodexID, CodexBackend{}, opts) }

func permissionOrEdit(p string) string {
	if p == "" {
		return "edit"
	}
	return p
}

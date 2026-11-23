package players

// Google AI Pro / Ultra through the official Antigravity CLI (`agy`, headless stream-json)
// (orchestra/players/antigravity.py).
//
// Event shapes recorded from agy 1.5.14 on 2026-11-21 (tests/fixtures/agy_*.jsonl):
//
//	{"event":"init","conversation_id":...,"init":{"cwd","tools","permission_mode"}}
//	{"event":"step_update","step_update":{"conversation_id","step_index",
//	    "state":"ACTIVE"|"DONE"|"ERROR",
//	    "step_type":"user_input"|"agent_response"|"tool"|"system_message"|...,
//	    "tool_name","text_delta","usage","tool_info":{"name","parameters","output"?,"error"?}}}
//	{"event":"result","result":{"conversation_id","status":"SUCCESS"|"ERROR"|"CANCELED"|...,
//	    "response","error"?,"num_turns","usage":{...},"denied_actions"?:[{"action","display_name"}]}}
//
// What the real runs showed, and the docs did not say:
//   - text_delta is always incremental, including on the DONE event (which carries the tail,
//     often "\n"). Every delta is appended; none is a repeat.
//   - A failed or denied tool ends with state "ERROR" and tool_info.error, not "DONE".
//   - usage.output_tokens already includes thinking_tokens; input_tokens excludes cache_read_tokens.
//   - Headless agy cannot ask for permission. A denied command ends the turn with an empty
//     response and status SUCCESS, listing it in denied_actions.
//   - Modes: `--mode accept-edits` and `--mode plan` deny every shell command headlessly;
//     `--mode auto` is not available on every account. The default mode with `--sandbox` runs
//     commands under terminal restrictions and allows edits, which matches Codex's workspace-write.

import (
	"strconv"
	"strings"
)

// Antigravity identity and hints (AntigravityPlayer's class attributes).
const (
	AntigravityID          = "antigravity"
	AntigravityDisplayName = "Antigravity (Google AI Pro)"
	AntigravityExecutable  = "agy"
	AntigravityInstallHint = "irm https://antigravity.google/cli/install.ps1 | iex   (PowerShell)"
	AntigravityLoginHint   = "agy   (sign in once with the Google account that has AI Pro)"
)

// Tool names from the init event's tool list plus edit_file and write_file seen in real runs.
var agyWriteTools = map[string]bool{
	"edit_file": true, "write_file": true, "write_to_file": true, "replace_file_content": true,
	"multi_replace_file_content": true, "sed_file": true, "notebook_edit": true,
}

var agyPathKeys = []string{"Path", "TargetFile", "AbsolutePath", "FilePath", "path", "file_path"}

const agyDeniedHint = "Antigravity stopped because it needed permission it cannot ask for in this mode. " +
	"Use --permission edit to let it run commands in its sandbox, or full for no limits."

func agyPathFrom(params map[string]any) string {
	for _, key := range agyPathKeys {
		if s, ok := params[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// AntigravityMapper maps agy stream-json lines.
type AntigravityMapper struct {
	MapperBase
	toolsStarted map[int]bool
	toolsDone    map[int]bool
	textSteps    map[int]bool // agent_response steps that have produced text
}

// NewAntigravityMapper returns a mapper rooted at cwd.
func NewAntigravityMapper(cwd string) *AntigravityMapper {
	return &AntigravityMapper{MapperBase: NewMapperBase(cwd),
		toolsStarted: map[int]bool{}, toolsDone: map[int]bool{}, textSteps: map[int]bool{}}
}

// Feed maps one JSON line.
func (m *AntigravityMapper) Feed(obj map[string]any) []Event {
	switch obj["event"] {
	case "init":
		m.Session, _ = obj["conversation_id"].(string)
		return []Event{NewEvent(SessionStarted, map[string]any{
			"backend_session_id": orNil(m.Session), "model": mapOf(obj, "init")["model"]})}
	case "step_update":
		return m.step(mapOf(obj, "step_update"))
	case "result":
		return m.result(mapOf(obj, "result"))
	}
	return nil
}

func (m *AntigravityMapper) result(res map[string]any) []Event {
	u := mapOf(res, "usage")
	cached := intOf(u, "cache_read_tokens")
	events := []Event{NewEvent(Usage, map[string]any{
		"input_tokens":        intOf(u, "input_tokens") + cached,
		"cached_input_tokens": cached,
		"output_tokens":       intOf(u, "output_tokens"), // already includes thinking tokens
		"cost_usd":            nil,                       // covered by the subscription
	})}
	if m.Session == "" {
		m.Session, _ = res["conversation_id"].(string)
	}

	var denied []string
	for _, d := range listOf(res, "denied_actions") {
		dm, _ := d.(map[string]any)
		name := "tool"
		if truthy(dm["display_name"]) {
			name = PyStr(dm["display_name"])
		} else if truthy(dm["action"]) {
			name = PyStr(dm["action"])
		}
		denied = append(denied, name)
		events = append(events, NewEvent(Notice, map[string]any{"message": "Permission denied: " + name}))
	}

	status := strings.ToUpper(strOf(res, "status", "SUCCESS"))
	switch status {
	case "SUCCESS":
		if !m.TextStarted() {
			if response := strOf(res, "response", ""); response != "" {
				events = append(events, m.Text(response, false)...)
			} else if len(denied) > 0 {
				// Headless agy stops the turn when it is refused; say why instead of an empty reply.
				events = append(events, NewEvent(Notice, map[string]any{"message": agyDeniedHint}))
			}
		}
		return append(events, m.Complete("end_turn")...)
	case "CANCELED", "CANCELLED", "INTERRUPTED":
		return append(events, m.Complete("cancelled")...)
	}
	msg := "Antigravity run ended with status " + status
	if truthy(res["error"]) {
		msg = PyStr(res["error"])
	}
	events = append(events, m.Err(msg)...)
	return append(events, m.Complete("error")...)
}

func (m *AntigravityMapper) step(step map[string]any) []Event {
	index := -1
	if v, ok := step["step_index"]; ok {
		switch x := v.(type) {
		case float64:
			index = int(x)
		case string:
			if n, err := strconv.Atoi(x); err == nil {
				index = n
			}
		}
	}
	state := strOf(step, "state", "")
	finished := state == "DONE" || state == "ERROR"

	switch step["step_type"] {
	case "agent_response":
		delta := strOf(step, "text_delta", "")
		if delta == "" {
			return nil
		}
		first := !m.textSteps[index]
		m.textSteps[index] = true
		return m.Text(delta, first)
	case "tool":
		return m.tool(step, index, state, finished)
	}
	return nil
}

func (m *AntigravityMapper) tool(step map[string]any, index int, state string, finished bool) []Event {
	info := mapOf(step, "tool_info")
	name := strOf(step, "tool_name", "")
	if name == "" {
		name = strOf(info, "name", "")
	}
	if name == "" {
		name = "tool"
	}
	params := mapOf(info, "parameters")
	tid := strconv.Itoa(index)
	var events []Event
	if !m.toolsStarted[index] {
		m.toolsStarted[index] = true
		events = append(events, NewEvent(ToolStarted, map[string]any{"id": tid, "name": name, "input": params}))
	}
	if finished && !m.toolsDone[index] {
		m.toolsDone[index] = true
		errVal := info["error"]
		ok := state == "DONE" && !truthy(errVal)
		if truthy(info["output"]) {
			events = append(events, NewEvent(ToolOutput, map[string]any{"id": tid, "chunk": PyStr(info["output"])}))
		} else if em, isMap := errVal.(map[string]any); isMap && truthy(em["message"]) {
			events = append(events, NewEvent(ToolOutput, map[string]any{"id": tid, "chunk": PyStr(em["message"])}))
		}
		if path := agyPathFrom(params); ok && agyWriteTools[name] && path != "" {
			events = append(events, NewEvent(FileChanged, map[string]any{"path": m.RelPath(path), "kind": "update"}))
		}
		events = append(events, NewEvent(ToolFinished, map[string]any{"id": tid, "ok": ok}))
	}
	return events
}

// AntigravityBackend builds headless `agy` commands.
type AntigravityBackend struct{}

// BuildCommand returns argv and one stream-json user message for stdin.
//
// The prompt goes on stdin as one stream-json message: no command-line length limit, and
// nothing in the prompt can be read as a flag. Closing stdin ends the session after this
// one turn.
func (AntigravityBackend) BuildCommand(prompt string, opts Options) ([]string, *string) {
	argv := []string{"agy", "--input-format", "stream-json", "--output-format", "stream-json"}
	switch permissionOrEdit(opts.Permission) {
	case "read-only":
		argv = append(argv, "--mode", "plan")
	case "edit":
		argv = append(argv, "--sandbox") // default mode + sandbox: edits and sandboxed commands
	default:
		argv = append(argv, "--dangerously-skip-permissions")
	}
	if opts.Model != "" {
		argv = append(argv, "--model", opts.Model)
	}
	if opts.SessionID != "" {
		argv = append(argv, "--conversation", opts.SessionID)
	}
	// Built by hand to keep Python's key order: {"event": "user", "message": {"content": ...}}.
	stdin := `{"event": "user", "message": {"content": ` + DumpsJSON(prompt) + "}}\n"
	return argv, &stdin
}

// NewMapper returns an Antigravity mapper.
func (AntigravityBackend) NewMapper(cwd string) Mapper { return NewAntigravityMapper(cwd) }

// NewAntigravity returns an Antigravity player.
func NewAntigravity(opts Options) (*CLIPlayer, error) {
	return NewCLIPlayer(AntigravityID, AntigravityBackend{}, opts)
}

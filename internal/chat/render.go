package chat

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/Adeel0-0/agent-orchestra/internal/players"
)

// Renderer prints events for a human. It keeps nothing but what it needs to lay out lines
// (orchestra/chat.py's Renderer).
type Renderer struct {
	out       io.Writer
	color     bool
	loginHint string
	cwd       string // "" means paths are shown as given
	midLine   bool
	usage     map[string]any
}

// Keys that name what a tool works on, in the order they are looked for.
var (
	pathKeys = []string{"file_path", "Path", "path", "TargetFile", "AbsolutePath", "notebook_path"}
	textKeys = []string{"command", "CommandLine", "pattern", "url"}
)

const (
	summaryWidth  = 100 // longest tool summary
	outputLines   = 4   // tool output lines shown (the last ones)
	outputLineMax = 160 // characters shown per tool output line
)

// NewRenderer writes to out. color nil means: on when out is a terminal and NO_COLOR is
// unset. cwd, when set, makes project paths relative to it.
func NewRenderer(out io.Writer, color *bool, loginHint, cwd string) *Renderer {
	useColor := false
	f, isFile := out.(*os.File)
	if color != nil {
		useColor = *color
	} else {
		useColor = isFile && isTerminal(f.Fd()) && os.Getenv("NO_COLOR") == ""
	}
	if useColor && isFile {
		enableANSI(f) // turns on escape handling in older Windows consoles
	}
	if cwd != "" {
		if abs, err := filepath.Abs(cwd); err == nil {
			cwd = abs
		}
	}
	return &Renderer{out: out, color: useColor, loginHint: loginHint, cwd: cwd}
}

func (r *Renderer) c(code, text string) string {
	if !r.color {
		return text
	}
	return "\033[" + code + "m" + text + "\033[0m"
}

func (r *Renderer) line(text string) {
	if r.midLine {
		io.WriteString(r.out, "\n")
		r.midLine = false
	}
	io.WriteString(r.out, text+"\n")
}

// shortPath shows project files relative to the project; long paths keep their end,
// where the file name is.
func (r *Renderer) shortPath(value string) string {
	if r.cwd != "" {
		if abs, err := filepath.Abs(value); err == nil {
			if rel, err := filepath.Rel(r.cwd, abs); err == nil && rel != ".." &&
				!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				value = rel
			}
		}
	}
	if runes := []rune(value); len(runes) > summaryWidth {
		value = "..." + string(runes[len(runes)-(summaryWidth-3):])
	}
	return value
}

// shortText keeps a summary on one line: a multi-line command would otherwise break the layout.
func shortText(value string) string {
	var parts []string
	for _, part := range splitLines(value) {
		if p := strings.TrimSpace(part); p != "" {
			parts = append(parts, p)
		}
	}
	value = strings.Join(parts, " ; ")
	if runes := []rune(value); len(runes) > summaryWidth {
		value = string(runes[:summaryWidth-3]) + "..."
	}
	return value
}

func (r *Renderer) summary(data map[string]any) string {
	raw := data["input"]
	if !truthy(raw) {
		raw = map[string]any{}
	}
	inp, isMap := raw.(map[string]any)
	if !isMap {
		return shortText(players.PyStr(raw))
	}
	for _, key := range textKeys {
		if truthy(inp[key]) {
			return shortText(players.PyStr(inp[key]))
		}
	}
	for _, key := range pathKeys {
		if s, ok := inp[key].(string); ok && s != "" {
			return r.shortPath(s)
		}
	}
	if truthy(inp["paths"]) {
		var shown []string
		for _, p := range anyList(inp["paths"]) {
			shown = append(shown, r.shortPath(players.PyStr(p)))
		}
		return shortText(strings.Join(shown, ", "))
	}
	text := players.DumpsJSON(inp)
	if text == "{}" {
		return ""
	}
	return shortText(text)
}

// Show prints one event.
func (r *Renderer) Show(evt players.Event) {
	d := evt.Data
	switch evt.Type {
	case players.TextDelta:
		text := evt.Str("text")
		io.WriteString(r.out, text)
		r.midLine = !strings.HasSuffix(text, "\n")
	case players.ToolStarted:
		head := r.c("36", "  > "+players.PyStr(d["name"]))
		if s := r.summary(d); s != "" {
			head += " " + r.c("2", s)
		}
		r.line(head)
	case players.ToolOutput:
		chunk := ""
		if v, ok := d["chunk"]; ok {
			chunk = players.PyStr(v)
		}
		lines := splitLines(strings.TrimRightFunc(chunk, unicode.IsSpace))
		shown := lines
		if len(shown) > outputLines {
			shown = shown[len(shown)-outputLines:]
			r.line(r.c("2", fmt.Sprintf("    | ... %d more lines", len(lines)-len(shown))))
		}
		for _, l := range shown {
			if runes := []rune(l); len(runes) > outputLineMax {
				l = string(runes[:outputLineMax])
			}
			r.line(r.c("2", "    | "+l))
		}
	case players.ToolFinished:
		if !truthy(d["ok"]) {
			r.line(r.c("31", "    x failed"))
		}
	case players.FileChanged:
		r.line(r.c("33", fmt.Sprintf("  ~ %s (%s)", players.PyStr(d["path"]), players.PyStr(d["kind"]))))
	case players.Notice:
		r.line(r.c("33", "  ! "+players.PyStr(d["message"])))
	case players.PermissionRequested:
		r.line(fmt.Sprintf("  ? %s: %s", players.PyStr(d["tool"]), players.DumpsJSON(d["input"])))
	case players.Error:
		r.line(r.c("31", "error: "+players.PyStr(d["message"])))
		if d["kind"] == "auth" && r.loginHint != "" {
			r.line("  fix: " + r.loginHint)
		}
	case players.Usage:
		r.usage = d
	case players.TurnCompleted:
		r.turnCompleted(d)
	}
}

func (r *Renderer) turnCompleted(d map[string]any) {
	var parts []string
	if len(r.usage) > 0 {
		parts = append(parts, fmt.Sprintf("%s in / %s out tokens", usageCount(r.usage, "input_tokens"), usageCount(r.usage, "output_tokens")))
		if cost, ok := toFloat(r.usage["cost_usd"]); ok {
			parts = append(parts, fmt.Sprintf("$%.6f", cost))
		}
	}
	if reason, ok := d["stop_reason"]; ok && reason != nil && reason != "end_turn" {
		parts = append(parts, players.PyStr(reason))
	}
	if len(parts) > 0 {
		r.line(r.c("2", "["+strings.Join(parts, ", ")+"]"))
	} else {
		r.line("")
	}
	r.usage = nil
}

// stopped prints the marker for a turn the user interrupted.
func (r *Renderer) stopped() { r.line(r.c("2", "[stopped]")) }

func usageCount(usage map[string]any, key string) string {
	v, ok := usage[key]
	if !ok {
		return "0"
	}
	return players.PyStr(v)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func anyList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	}
	return []any{v}
}

// truthy is Python's bool(v) for event data.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case []any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// splitLines is Python's str.splitlines(): every line boundary Python knows, and no
// empty last element for a trailing newline.
func splitLines(s string) []string {
	var lines []string
	start := 0
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			lines = append(lines, string(runes[start:i]))
			start = i + 1
		case '\r':
			lines = append(lines, string(runes[start:i]))
			if i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(runes) {
		lines = append(lines, string(runes[start:]))
	}
	return lines
}

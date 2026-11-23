// Package registry lists the players that exist and checks whether each one is installed
// and logged in on this machine (orchestra/players/registry.py).
package registry

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Adeel0-0/agent-orchestra/internal/players"
	"github.com/Adeel0-0/agent-orchestra/internal/runner"
)

// checkTimeout bounds `--version` and the login check (Python's _run timeout).
const checkTimeout = 30 * time.Second

// Spec describes one player: how to construct it and how to check it.
type Spec struct {
	ID          string
	DisplayName string
	Executable  string // looked up on PATH by Check; "" for players without a CLI
	InstallHint string
	LoginHint   string
	// LoginCheck is a command whose exit code 0 means logged in (argv[0] is replaced by
	// the resolved executable). nil means the CLI has no way to check.
	LoginCheck []string
	// New constructs the player for one session. It validates options (permission,
	// model, resume id) before any health check runs.
	New func(players.Options) (players.Player, error)
	// Health, when set, replaces the executable/login check (OpenRouter checks the
	// keyring for a key instead).
	Health func() Health
}

// Health is what Check found (Python's PlayerHealth).
type Health struct {
	ID       string
	Name     string
	Path     string // "" means not installed (Python None)
	Version  string // "" means unknown (Python None)
	LoggedIn *bool  // nil means this CLI has no way to check
	Fix      string // the command that would fix the first problem found; "" if none
}

// Ready reports whether the player can run: installed, and not known to be logged out.
func (h Health) Ready() bool {
	return h.Path != "" && (h.LoggedIn == nil || *h.LoggedIn)
}

// ToMap is the JSON form used by `orchestra doctor --json` and the app server:
// Python's {**asdict(h), "ready": h.ready}, with "" fields as null.
func (h Health) ToMap() map[string]any {
	var loggedIn any
	if h.LoggedIn != nil {
		loggedIn = *h.LoggedIn
	}
	return map[string]any{
		"id": h.ID, "name": h.Name, "path": nullable(h.Path), "version": nullable(h.Version),
		"logged_in": loggedIn, "fix": nullable(h.Fix), "ready": h.Ready(),
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Bool returns a pointer to b, for Health.LoggedIn.
func Bool(b bool) *bool { return &b }

func cliNew(ctor func(players.Options) (*players.CLIPlayer, error)) func(players.Options) (players.Player, error) {
	return func(o players.Options) (players.Player, error) {
		p, err := ctor(o)
		if err != nil {
			return nil, err
		}
		return p, nil
	}
}

// builtin returns the players in Python's PLAYERS order.
func builtin() []Spec {
	return []Spec{
		{
			ID: players.CodexID, DisplayName: players.CodexDisplayName, Executable: players.CodexExecutable,
			InstallHint: players.CodexInstallHint, LoginHint: players.CodexLoginHint,
			LoginCheck: players.CodexLoginCheck, New: cliNew(players.NewCodex),
		},
		{
			ID: players.AntigravityID, DisplayName: players.AntigravityDisplayName,
			Executable:  players.AntigravityExecutable,
			InstallHint: players.AntigravityInstallHint, LoginHint: players.AntigravityLoginHint,
			LoginCheck: nil, New: cliNew(players.NewAntigravity),
		},
		{
			ID: players.ClaudeID, DisplayName: players.ClaudeDisplayName, Executable: players.ClaudeExecutable,
			InstallHint: players.ClaudeInstallHint, LoginHint: players.ClaudeLoginHint,
			LoginCheck: players.ClaudeLoginCheck, New: cliNew(players.NewClaude),
		},
		// TODO(lead): OpenRouter goes last, as in Python. Either append its Spec here or
		// call Register from the OpenRouter package's wiring. Its Spec sets New and a
		// Health func that reports key presence from the keyring:
		//   key present: Path "https://openrouter.ai/api/v1",
		//                Version "Public HTTPS API (key presence only; credit/model access not checked)",
		//                LoggedIn true
		//   no key:      same Path/Version, LoggedIn false, Fix LoginHint
		//   keyring error: Path "", LoggedIn false, Fix the error text
	}
}

var (
	mu    sync.RWMutex
	specs = builtin()
)

// Register appends a player (or replaces one with the same ID, keeping its position).
func Register(spec Spec) {
	mu.Lock()
	defer mu.Unlock()
	for i, s := range specs {
		if s.ID == spec.ID {
			specs[i] = spec
			return
		}
	}
	specs = append(specs, spec)
}

// All returns every registered player in order.
func All() []Spec {
	mu.RLock()
	defer mu.RUnlock()
	return append([]Spec(nil), specs...)
}

// IDs returns the registered player ids in order.
func IDs() []string {
	all := All()
	ids := make([]string, len(all))
	for i, s := range all {
		ids[i] = s.ID
	}
	return ids
}

// Get returns the player with this id.
func Get(id string) (Spec, error) {
	for _, s := range All() {
		if s.ID == id {
			return s, nil
		}
	}
	return Spec{}, fmt.Errorf("Unknown player '%s'. Known: %s", id, strings.Join(IDs(), ", "))
}

// Check reports whether the player is installed and logged in.
func Check(spec Spec) Health {
	if spec.Health != nil {
		return spec.Health()
	}
	h := Health{ID: spec.ID, Name: spec.DisplayName}
	path := runner.Resolve(spec.Executable)
	if spec.Executable == "" || path == "" {
		h.Fix = spec.InstallHint
		return h
	}
	h.Path = path
	h.Version = versionLine(run(path, "--version"))

	if spec.LoginCheck == nil {
		return h
	}
	loggedIn := LoggedIn(run(path, spec.LoginCheck[1:]...))
	h.LoggedIn = &loggedIn
	if !loggedIn {
		h.Fix = spec.LoginHint
	}
	return h
}

// CheckAll checks every registered player, in order.
func CheckAll() []Health {
	all := All()
	out := make([]Health, len(all))
	for i, s := range all {
		out[i] = Check(s)
	}
	return out
}

// run executes the CLI directly (no shell; the arguments are fixed constants) with stdin
// closed, killing its process tree after checkTimeout.
func run(path string, args ...string) runner.Result {
	return runner.New("").Run(runner.ArgsCommand(append([]string{path}, args...)...), runner.Options{Timeout: checkTimeout})
}

// ran reports whether the process started and exited on its own (Python's _run did not
// return None).
func ran(res runner.Result) bool { return res.ExitCode != -1 }

// versionLine is the first line of stdout, or of stderr when stdout is empty.
func versionLine(res runner.Result) string {
	if !ran(res) {
		return ""
	}
	text := res.Stdout
	if text == "" {
		text = res.Stderr
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return strings.TrimRight(strings.SplitN(text, "\n", 2)[0], "\r")
}

// LoggedIn reads a login check's result: exit code 0, and either `claude auth status`'s
// JSON loggedIn or no "not logged in" in the output.
func LoggedIn(res runner.Result) bool {
	if !ran(res) || res.ExitCode != 0 {
		return false
	}
	var data any
	if json.Unmarshal([]byte(res.Stdout), &data) == nil {
		if obj, ok := data.(map[string]any); ok {
			if v, has := obj["loggedIn"]; has {
				return pyTruthy(v)
			}
		}
	}
	return !strings.Contains(strings.ToLower(res.Stdout+res.Stderr), "not logged in")
}

func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

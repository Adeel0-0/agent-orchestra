package main

// Commands for the agent backend (orchestra/cli.py on main): doctor, chat, serve, ui.
// OpenRouter commands (models, openrouter-key) live in openrouter_cmds.go.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/Adeel0-0/agent-orchestra/internal/app"
	"github.com/Adeel0-0/agent-orchestra/internal/chat"
	"github.com/Adeel0-0/agent-orchestra/internal/players"
	"github.com/Adeel0-0/agent-orchestra/internal/pyjson"
	"github.com/Adeel0-0/agent-orchestra/internal/registry"
	webassets "github.com/Adeel0-0/agent-orchestra/orchestra/app/web"
)

func backendCommands() []command {
	dataDir := flagSpec{Name: "data-dir", Kind: "string", Help: "Private data directory (default: ~/.orchestra)"}
	return []command{
		{name: "doctor", help: "Check which agent CLIs are installed and logged in", usage: "doctor [--json]", stopAfter: -1,
			flags: []flagSpec{{Name: "json", Kind: "bool"}}, run: cmdDoctor},
		{name: "serve", help: "Start the protected local HTTP/WebSocket API (no UI)", usage: "serve [--data-dir DATA_DIR]", stopAfter: -1,
			flags: []flagSpec{dataDir}, run: cmdServe},
		{name: "ui", help: "Open the local agent workspace in your browser", usage: "ui [--data-dir DATA_DIR] [--browser]", stopAfter: -1,
			flags: []flagSpec{dataDir, {Name: "browser", Kind: "bool", Help: "Accepted for compatibility; the Go version always uses the browser"}}, run: cmdUI},
		{name: "chat", help: "Chat with an agent in a project folder",
			usage: "chat [PROMPT ...] [--agent AGENT] [--model MODEL] [--permission {read-only,edit,full}] [--cwd CWD] [--resume RESUME] [--json]", stopAfter: -1,
			flags: []flagSpec{
				{Name: "agent", Short: "a", Kind: "string", Default: "codex"},
				{Name: "model", Short: "m", Kind: "string"},
				{Name: "permission", Short: "p", Kind: "string", Default: "edit", Choices: []string{"read-only", "edit", "full"}},
				{Name: "cwd", Short: "C", Kind: "string", Default: "."},
				{Name: "resume", Short: "r", Kind: "string"},
				{Name: "json", Kind: "bool"},
			}, run: cmdChat},
	}
}

// doctorRow keeps Python's key order for `doctor --json` ({**PlayerHealth.__dict__, "ready"}).
type doctorRow struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Path     *string `json:"path"`
	Version  *string `json:"version"`
	LoggedIn *bool   `json:"logged_in"`
	Fix      *string `json:"fix"`
	Ready    bool    `json:"ready"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func cmdDoctor(p parsed, out, errOut io.Writer) int {
	results := registry.CheckAll()
	anyReady := false
	for _, r := range results {
		anyReady = anyReady || r.Ready()
	}
	code := 1
	if anyReady {
		code = 0
	}
	if p.bools["json"] {
		rows := make([]doctorRow, len(results))
		for i, r := range results {
			rows[i] = doctorRow{ID: r.ID, Name: r.Name, Path: optional(r.Path), Version: optional(r.Version),
				LoggedIn: r.LoggedIn, Fix: optional(r.Fix), Ready: r.Ready()}
		}
		data, err := pyjson.MarshalIndent(rows)
		if err != nil {
			fmt.Fprintf(errOut, "[FAILED] %s\n", err)
			return 1
		}
		fmt.Fprintln(out, string(data))
		return code
	}
	for _, r := range results {
		state := "ready"
		switch {
		case r.Path == "":
			state = "not installed"
		case r.LoggedIn != nil && !*r.LoggedIn:
			state = "not logged in"
		case r.LoggedIn == nil:
			state = "installed (login not checkable; run it once to sign in)"
		}
		fmt.Fprintf(out, "%-30s %s\n", r.Name, state)
		if r.Version != "" {
			fmt.Fprintf(out, "%-30s %s\n", "", r.Version)
		}
		if r.Fix != "" {
			fmt.Fprintf(out, "%-30s fix: %s\n", "", r.Fix)
		}
	}
	return code
}

func cmdChat(p parsed, out, errOut io.Writer) int {
	var prompt *string
	if len(p.positional) > 0 {
		joined := strings.Join(p.positional, " ")
		prompt = &joined
	}
	return chat.Run(chat.Options{
		Agent: p.str("agent"), Prompt: prompt, Cwd: p.str("cwd"), Model: p.str("model"),
		Permission: p.str("permission"), Resume: p.str("resume"), JSON: p.bools["json"],
	}, os.Stdin, out, errOut)
}

// playerFactory builds the player for an app session, as Python's make_player does:
// CLI players resume from the saved backend session id; OpenRouter resumes from its
// saved conversation state instead.
func playerFactory(s app.Session, cwd string) (players.Player, error) {
	spec, err := registry.Get(s.Player)
	if err != nil {
		return nil, err
	}
	opts := players.Options{Cwd: cwd, Permission: s.Permission}
	if s.Model != nil {
		opts.Model = *s.Model
	}
	if s.Player != "openrouter" && s.BackendSessionID != nil {
		opts.SessionID = *s.BackendSessionID
	}
	return spec.New(opts)
}

func playerHealth(context.Context) []app.PlayerHealth {
	results := registry.CheckAll()
	out := make([]app.PlayerHealth, len(results))
	for i, r := range results {
		out[i] = app.PlayerHealth{ID: r.ID, Name: r.Name, Path: optional(r.Path), Version: optional(r.Version),
			LoggedIn: r.LoggedIn, Fix: optional(r.Fix)}
	}
	return out
}

// appDependencies is what the server needs from the rest of the program.
func appDependencies(withWeb bool) app.Config {
	cfg := app.Config{Factory: playerFactory, PlayerHealth: playerHealth}
	addOpenRouterDependencies(&cfg)
	if withWeb {
		cfg.Web = webassets.FS()
	}
	return cfg
}

func launchFailed(err error, errOut io.Writer) int {
	if errors.Is(err, context.Canceled) {
		return 130
	}
	fmt.Fprintf(errOut, "[FAILED] %s\n", err)
	return 2
}

func cmdServe(p parsed, out, errOut io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := app.Serve(ctx, app.LaunchConfig{DataDir: p.str("data-dir"), Out: out, App: appDependencies(false)})
	if err != nil {
		return launchFailed(err, errOut)
	}
	return 0
}

func cmdUI(p parsed, out, errOut io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := app.RunUI(ctx, app.LaunchConfig{DataDir: p.str("data-dir"), Out: out, App: appDependencies(true)})
	if err != nil {
		return launchFailed(err, errOut)
	}
	return 0
}

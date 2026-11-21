// Command orchestra is the Go port of the Python CLI (orchestra/cli.py): the same
// subcommands, flags, defaults, [OK]/[FAILED] messages and exit codes
// (0 success, 1 refused, 2 usage error).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/Adeel0-0/agent-orchestra/internal/bus"
	"github.com/Adeel0-0/agent-orchestra/internal/config"
	"github.com/Adeel0-0/agent-orchestra/internal/daemon"
	"github.com/Adeel0-0/agent-orchestra/internal/model"
	"github.com/Adeel0-0/agent-orchestra/internal/pyjson"
	"github.com/Adeel0-0/agent-orchestra/internal/runner"
)

type command struct {
	name      string
	help      string
	usage     string
	flags     []flagSpec
	stopAfter int // positionals after which the rest is passed through (run); -1 = never
	run       func(p parsed, out, errOut io.Writer) int
}

func commands() []command {
	return []command{
		{name: "status", help: "Show current state and lock information", usage: "status [--json]", stopAfter: -1,
			flags: []flagSpec{{Name: "json", Kind: "bool"}}, run: cmdStatus},
		{name: "claim", help: "Claim writer lock for a task", usage: "claim TASK_ID [--agent AGENT] [--scope SCOPE] [--lease LEASE] [--force]", stopAfter: -1,
			flags: []flagSpec{{Name: "agent", Kind: "string", Default: "agent"}, {Name: "scope", Kind: "string"},
				{Name: "lease", Kind: "int", Default: "1800"}, {Name: "force", Kind: "bool"}}, run: cmdClaim},
		{name: "release", help: "Release writer lock and request handoff", usage: "release [--agent AGENT] [--status {complete,paused,failed}] [--summary SUMMARY] [--force]", stopAfter: -1,
			flags: []flagSpec{{Name: "agent", Kind: "string"}, {Name: "status", Kind: "string", Default: "complete", Choices: []string{"complete", "paused", "failed"}},
				{Name: "summary", Kind: "string"}, {Name: "force", Kind: "bool"}}, run: cmdRelease},
		{name: "emit", help: "Emit a custom event on the bus", usage: "emit EVENT_TYPE [PAYLOAD] [--agent AGENT]", stopAfter: -1,
			flags: []flagSpec{{Name: "agent", Kind: "string", Default: "cli"}}, run: cmdEmit},
		{name: "listen", help: "Stream incoming events live", usage: "listen [--all]", stopAfter: -1,
			flags: []flagSpec{{Name: "all", Kind: "bool"}}, run: cmdListen},
		{name: "run", help: "Run a Python script through the orchestra runner", usage: "run SCRIPT [ARGS ...]", stopAfter: 1, run: cmdRun},
		{name: "start", help: "Start the autonomous orchestrator daemon", usage: "start [--interval INTERVAL]", stopAfter: -1,
			flags: []flagSpec{{Name: "interval", Kind: "float", Default: "2.0"}}, run: cmdStart},
		{name: "init", help: "Create a starter orchestra.json configuration", usage: "init [--force]", stopAfter: -1,
			flags: []flagSpec{{Name: "force", Kind: "bool"}}, run: cmdInit},
	}
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, out, errOut io.Writer) int {
	cmds := commands()
	if len(args) == 0 {
		usage(errOut, cmds)
		fmt.Fprintln(errOut, "orchestra: error: the following arguments are required: command")
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" {
		usage(out, cmds)
		return 0
	}
	for _, c := range cmds {
		if c.name != args[0] {
			continue
		}
		rest := args[1:]
		for _, a := range rest {
			if a == "-h" || a == "--help" {
				fmt.Fprintf(out, "usage: orchestra %s\n\n%s\n", c.usage, c.help)
				return 0
			}
			if a == "--" {
				break
			}
		}
		p, err := parseArgs(rest, c.flags, c.stopAfter)
		if err == nil {
			err = checkPositionals(c.name, p)
		}
		if err != nil {
			fmt.Fprintf(errOut, "usage: orchestra %s\norchestra %s: error: %s\n", c.usage, c.name, err)
			return 2
		}
		return c.run(p, out, errOut)
	}
	usage(errOut, cmds)
	fmt.Fprintf(errOut, "orchestra: error: argument command: invalid choice: '%s'\n", args[0])
	return 2
}

func checkPositionals(name string, p parsed) error {
	n := len(p.positional)
	switch name {
	case "claim":
		if n < 1 {
			return usageError{"the following arguments are required: task_id"}
		}
		if n > 1 {
			return usageError{"unrecognized arguments: " + strings.Join(p.positional[1:], " ")}
		}
	case "emit":
		if n < 1 {
			return usageError{"the following arguments are required: event_type"}
		}
		if n > 2 {
			return usageError{"unrecognized arguments: " + strings.Join(p.positional[2:], " ")}
		}
	case "run":
		if n < 1 {
			return usageError{"the following arguments are required: script, args"}
		}
	default:
		if n > 0 {
			return usageError{"unrecognized arguments: " + strings.Join(p.positional, " ")}
		}
	}
	return nil
}

func usage(w io.Writer, cmds []command) {
	fmt.Fprintln(w, "usage: orchestra {status,claim,release,emit,listen,run,start,init} ...")
	fmt.Fprintln(w, "\nAutonomous Multi-Agent Orchestration & IPC Event Bus (Go binary)\n\ncommands:")
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.help)
	}
}

// loadConfig loads orchestra.json from the current directory. Phase 5 is halted,
// so a config problem is exit 1 with a message, not Python's exit 2.
func loadConfig(errOut io.Writer) (config.Config, bool) {
	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(errOut, "[FAILED] Invalid configuration: %s\n", err)
		return cfg, false
	}
	return cfg, true
}

func orchestraDir(cfg config.Config) string {
	return filepath.Join(cfg.WorkspaceRoot, cfg.OrchestraDir)
}

func openBus(cfg config.Config, errOut io.Writer) (*bus.LockManager, *bus.EventBus, bool) {
	locks, err := bus.NewLockManager(orchestraDir(cfg))
	if err != nil {
		fmt.Fprintf(errOut, "[FAILED] %s\n", err)
		return nil, nil, false
	}
	b, err := bus.NewEventBus(orchestraDir(cfg))
	if err != nil {
		fmt.Fprintf(errOut, "[FAILED] %s\n", err)
		return nil, nil, false
	}
	return locks, b, true
}

// pyBool and pyVal print values the way Python's f-strings do.
func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func pyVal(m map[string]any, key string) string { return model.PyStr(m, key, "None") }

func cmdStatus(p parsed, out, errOut io.Writer) int {
	cfg, ok := loadConfig(errOut)
	if !ok {
		return 1
	}
	d, err := daemon.New(cfg)
	if err != nil {
		fmt.Fprintf(errOut, "[FAILED] %s\n", err)
		return 1
	}
	st := d.GetStatus()
	if p.bools["json"] {
		data, err := pyjson.MarshalIndent(st)
		if err != nil {
			fmt.Fprintf(errOut, "[FAILED] %s\n", err)
			return 1
		}
		fmt.Fprintln(out, string(data))
		return 0
	}
	fmt.Fprintln(out, "=== AGENT ORCHESTRA STATUS ===")
	fmt.Fprintf(out, "State:        %s\n", st.State)
	fmt.Fprintf(out, "Is Locked:    %s\n", pyBool(st.IsLocked))
	if st.ActiveLock != nil {
		fmt.Fprintf(out, "  Task:       %s\n", pyVal(st.ActiveLock, "task_id"))
		fmt.Fprintf(out, "  Agent:      %s\n", pyVal(st.ActiveLock, "agent_id"))
		fmt.Fprintf(out, "  Scope:      %s\n", pyVal(st.ActiveLock, "scope"))
	}
	if st.Git != nil {
		fmt.Fprintf(out, "Git Branch:   %s (HEAD: %s)\n", st.Git.Branch, st.Git.HeadSHA)
		fmt.Fprintf(out, "Git Clean:    %s (%d changed files)\n", pyBool(st.Git.IsClean), st.Git.ChangedCount)
	}
	if st.LatestEvent != nil {
		fmt.Fprintf(out, "Latest Event: %s by %s\n", st.LatestEvent.EventType, st.LatestEvent.SourceAgent)
	}
	return 0
}

func cmdClaim(p parsed, out, errOut io.Writer) int {
	cfg, ok := loadConfig(errOut)
	if !ok {
		return 1
	}
	locks, b, ok := openBus(cfg, errOut)
	if !ok {
		return 1
	}
	task, agent := p.positional[0], p.str("agent")
	claimed, err := daemon.Claim(locks, b, task, agent, p.str("scope"), int(p.num("lease")), p.bools["force"])
	if err != nil {
		fmt.Fprintf(errOut, "[FAILED] %s\n", err)
		return 1
	}
	if !claimed {
		current := locks.CurrentLock()
		holder, holdTask := "unknown", "unknown"
		if current != nil {
			holder, holdTask = pyVal(current, "agent_id"), pyVal(current, "task_id")
		}
		fmt.Fprintf(errOut, "[FAILED] Cannot acquire lock: held by agent '%s' for task '%s'. Use --force to override.\n", holder, holdTask)
		return 1
	}
	fmt.Fprintf(out, "[OK] Agent '%s' claimed writer lock for task '%s'.\n", agent, task)
	return 0
}

func cmdRelease(p parsed, out, errOut io.Writer) int {
	cfg, ok := loadConfig(errOut)
	if !ok {
		return 1
	}
	locks, b, ok := openBus(cfg, errOut)
	if !ok {
		return 1
	}
	agent := p.str("agent")
	res, err := daemon.Release(locks, b, agent, p.str("status"), p.str("summary"), p.bools["force"])
	if err != nil {
		fmt.Fprintf(errOut, "[FAILED] %s\n", err)
		return 1
	}
	if !res.Released {
		holder, asked := res.HoldingAgent, agent
		if holder == "" {
			holder = "None"
		}
		if asked == "" {
			asked = "None"
		}
		fmt.Fprintf(errOut, "[FAILED] Cannot release lock: held by '%s', not '%s'. Use --force to override.\n", holder, asked)
		return 1
	}
	fmt.Fprintf(out, "[OK] Writer lock released for task '%s'. Event emitted.\n", res.TaskID)
	return 0
}

func cmdEmit(p parsed, out, errOut io.Writer) int {
	cfg, ok := loadConfig(errOut)
	if !ok {
		return 1
	}
	_, b, ok := openBus(cfg, errOut)
	if !ok {
		return 1
	}
	payload := map[string]any{}
	if len(p.positional) > 1 && p.positional[1] != "" {
		var v any
		if err := json.Unmarshal([]byte(p.positional[1]), &v); err != nil {
			fmt.Fprintf(errOut, "[FAILED] Invalid JSON payload: %s\n", err)
			return 1
		}
		obj, isObj := v.(map[string]any)
		if !isObj {
			fmt.Fprintln(errOut, "[FAILED] Invalid JSON payload: payload must be a JSON object")
			return 1
		}
		payload = obj
	}
	evt, err := b.Emit(p.positional[0], p.str("agent"), payload)
	if err != nil {
		fmt.Fprintf(errOut, "[FAILED] %s\n", err)
		return 1
	}
	fmt.Fprintf(out, "[OK] Emitted event %s (%s)\n", evt.EventID, evt.EventType)
	return 0
}

func cmdListen(p parsed, out, errOut io.Writer) int {
	cfg, ok := loadConfig(errOut)
	if !ok {
		return 1
	}
	_, b, ok := openBus(cfg, errOut)
	if !ok {
		return 1
	}
	fmt.Fprintln(out, "[LISTEN] Listening to Agent Orchestra event bus (Ctrl+C to stop)...")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := b.Stream(ctx, 500*time.Millisecond, p.bools["all"], func(evt model.AgentEvent) {
		payload, _ := pyjson.Marshal(evt.Payload)
		fmt.Fprintf(out, "[%s] (%s) -> %s\n", evt.EventType, evt.SourceAgent, payload)
		if f, isFile := out.(*os.File); isFile {
			f.Sync()
		}
	})
	if err != nil {
		fmt.Fprintf(errOut, "[FAILED] %s\n", err)
		return 1
	}
	fmt.Fprintln(out, "\nStopped.")
	return 0
}

func cmdRun(p parsed, out, errOut io.Writer) int {
	cfg, ok := loadConfig(errOut)
	if !ok {
		return 1
	}
	script := p.positional[0]
	fmt.Fprintf(out, "[RUN] Running script: %s...\n", script)
	res := runner.New(cfg.WorkspaceRoot).RunScript(script, p.positional[1:], runner.Options{})
	if s := strings.TrimSpace(res.Stdout); s != "" {
		fmt.Fprintln(out, s)
	}
	if s := strings.TrimSpace(res.Stderr); s != "" {
		fmt.Fprintln(errOut, s)
	}
	return res.ExitCode
}

func cmdStart(p parsed, out, errOut io.Writer) int {
	cfg, ok := loadConfig(errOut)
	if !ok {
		return 1
	}
	d, err := daemon.New(cfg)
	if err != nil {
		fmt.Fprintf(errOut, "[FAILED] %s\n", err)
		return 1
	}
	fmt.Fprintf(out, "[ORCHESTRA] Agent Orchestra Conductor started (watching %s)...\n", pyList(cfg.WatchPaths))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	interval := time.Duration(p.num("interval") * float64(time.Second))
	if err := d.Start(ctx, interval); err != nil {
		fmt.Fprintf(errOut, "[FAILED] %s\n", err)
		return 1
	}
	return 0
}

func cmdInit(p parsed, out, errOut io.Writer) int {
	target := "orchestra.json"
	if _, err := os.Stat(target); err == nil && !p.bools["force"] {
		fmt.Fprintln(out, "orchestra.json already exists. Use --force to overwrite.")
		return 1
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(errOut, "[FAILED] %s\n", err)
		return 1
	}
	if err := config.Save(config.Default(), target); err != nil {
		fmt.Fprintf(errOut, "[FAILED] %s\n", err)
		return 1
	}
	fmt.Fprintln(out, "[OK] Created orchestra.json with starter defaults.")
	return 0
}

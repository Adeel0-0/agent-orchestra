// Package runner executes pipeline commands with a timeout that kills the whole
// process tree (orchestra/runner.py). Each command runs in its own process group
// (a Job Object on Windows), so `cmd.exe -> npm -> node` dies together.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTimeout matches Python's timeout_seconds=300.
	DefaultTimeout = 300 * time.Second
	// pipeGrace bounds the wait for output pipes after a kill (Python waits 5s).
	pipeGrace = 5 * time.Second
	// payloadTailBytes keeps event payloads small; Python stores whole outputs.
	payloadTailBytes = 64 * 1024
)

// Command is either a shell string (Shell) or a direct argument list (Args).
type Command struct {
	Shell string
	Args  []string
}

// ShellCommand runs s through cmd.exe /c on Windows and /bin/sh -c elsewhere.
func ShellCommand(s string) Command { return Command{Shell: s} }

// ArgsCommand runs args[0] directly with no shell.
func ArgsCommand(args ...string) Command { return Command{Args: args} }

func (c Command) display() string {
	if c.Args != nil {
		return strings.Join(c.Args, " ")
	}
	return c.Shell
}

// Options control one run. A zero Timeout means DefaultTimeout.
type Options struct {
	Cwd     string
	Env     map[string]string
	Timeout time.Duration
}

// Result mirrors Python's ExecutionResult.
type Result struct {
	Command         string
	ExitCode        int
	Stdout          string
	Stderr          string
	DurationSeconds float64
	Success         bool
	ParsedJSON      map[string]any
}

// Payload is the event form of the result (to_dict), with stdout and stderr cut
// to their last 64 KB so a noisy command cannot bloat events.jsonl.
func (r Result) Payload() map[string]any {
	var parsed any
	if r.ParsedJSON != nil {
		parsed = r.ParsedJSON
	}
	return map[string]any{
		"command":          r.Command,
		"exit_code":        r.ExitCode,
		"stdout":           tail(r.Stdout, payloadTailBytes),
		"stderr":           tail(r.Stderr, payloadTailBytes),
		"duration_seconds": r.DurationSeconds,
		"success":          r.Success,
		"parsed_json":      parsed,
	}
}

// tail returns the last max bytes of s without splitting a UTF-8 character.
func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := len(s) - max
	for cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut++
	}
	return s[cut:]
}

// Runner runs commands in DefaultCwd unless Options.Cwd says otherwise.
type Runner struct {
	DefaultCwd string
}

// New returns a runner rooted at cwd (the current directory if empty).
func New(cwd string) *Runner {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	return &Runner{DefaultCwd: abs}
}

// lockedBuffer is written by exec's copy goroutine and read after Wait; after a
// WaitDelay expiry the goroutine may still be finishing, so access is locked.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.ToValidUTF8(b.buf.String(), "�")
}

// Run executes cmd and never returns an error: failures are in the Result,
// as in Python (exit_code -1 and the reason in stderr).
func (r *Runner) Run(c Command, opts Options) (res Result) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cwd := opts.Cwd
	if cwd == "" {
		cwd = r.DefaultCwd
	}
	res = Result{Command: c.display(), ExitCode: -1}
	start := time.Now()
	defer func() {
		res.DurationSeconds = math.Round(time.Since(start).Seconds()*1000) / 1000
		res.ParsedJSON = ExtractJSON(res.Stdout)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd, err := buildCommand(ctx, c)
	if err != nil {
		res.Stderr = err.Error()
		return res
	}
	cmd.Dir = cwd
	cmd.Env = mergeEnv(opts.Env)
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	group, err := newProcessGroup(cmd)
	if err != nil {
		res.Stderr = err.Error()
		return res
	}
	defer group.close()
	cmd.Cancel = group.kill
	cmd.WaitDelay = pipeGrace

	if err := cmd.Start(); err != nil {
		res.Stderr = err.Error()
		return res
	}
	group.started(cmd.Process.Pid)

	waitErr := cmd.Wait()
	res.Stdout = stdout.String()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.Stderr = fmt.Sprintf("Command timed out after %s seconds.", formatSeconds(timeout))
		return res
	}
	res.Stderr = stderr.String()
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	} else if waitErr != nil {
		res.Stderr = waitErr.Error()
	}
	res.Success = res.ExitCode == 0
	return res
}

func formatSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}

func mergeEnv(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// ExtractJSON returns stdout as an object if the whole of it parses, otherwise
// the last line that is a JSON object (Python's _extract_json).
func ExtractJSON(text string) map[string]any {
	stripped := strings.TrimSpace(text)
	if stripped == "" {
		return nil
	}
	if obj, ok := parseObject(stripped); ok {
		return obj
	}
	lines := strings.Split(strings.ReplaceAll(stripped, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			if obj, ok := parseObject(line); ok {
				return obj
			}
		}
	}
	return nil
}

func parseObject(s string) (map[string]any, bool) {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, false
	}
	obj, ok := v.(map[string]any)
	return obj, ok
}

// RunScript runs a script the way `orchestra run` does: .py files with the
// Python found on PATH (python, python3, then the Windows "py -3" launcher),
// anything else directly.
func (r *Runner) RunScript(script string, args []string, opts Options) Result {
	path := script
	if !filepath.IsAbs(path) {
		base := opts.Cwd
		if base == "" {
			base = r.DefaultCwd
		}
		path = filepath.Join(base, path)
	}
	argv := append([]string{path}, args...)
	if strings.EqualFold(filepath.Ext(path), ".py") {
		interpreter, err := findPython()
		if err != nil {
			return Result{Command: strings.Join(argv, " "), ExitCode: -1, Stderr: err.Error()}
		}
		argv = append(interpreter, argv...)
	}
	return r.Run(ArgsCommand(argv...), opts)
}

func findPython() ([]string, error) {
	for _, name := range []string{"python", "python3"} {
		if p, err := exec.LookPath(name); err == nil {
			return []string{p}, nil
		}
	}
	if p, err := exec.LookPath("py"); err == nil {
		return []string{p, "-3"}, nil
	}
	return nil, errors.New("no Python interpreter found on PATH (tried python, python3, py)")
}

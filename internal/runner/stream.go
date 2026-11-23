package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// cmdUnsafe are characters cmd.exe treats as syntax. npm installs CLIs as .cmd
// shims that Windows runs through cmd.exe, so arguments to a shim must not
// contain them; text like prompts goes on stdin instead (orchestra/players/process.py).
const cmdUnsafe = "&|<>^%\"!\r\n"

// maxLine bounds one line of CLI output; a single JSON event can carry a large tool output.
const maxLine = 16 * 1024 * 1024

// ErrUnsafeShimArgument is returned when an argument to a .cmd/.bat file would be reinterpreted.
var ErrUnsafeShimArgument = errors.New("argument would be interpreted by cmd.exe")

// StreamOptions configure a streaming process.
type StreamOptions struct {
	Cwd   string
	Env   map[string]string
	Stdin *string // written then closed; nil means no stdin
}

// Process is a running command whose stdout is read line by line as it is printed.
// It runs in its own process group (a Job Object on Windows), so Kill ends the tree.
type Process struct {
	cmd    *exec.Cmd
	group  *processGroup
	stdout io.ReadCloser
	stderr *lockedBuffer

	waitOnce sync.Once
	exitCode int
	waitErr  error
	exited   atomic.Bool // set once Wait has reaped the process (no race on ProcessState)
	groupMu  sync.Mutex  // orders group.kill against group.close
	killOnce sync.Once
}

// Resolve returns the full path of an executable on PATH, or "" if not found.
func Resolve(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// StartStream starts argv with stdout piped for line reading and stderr collected.
func StartStream(argv []string, opts StreamOptions) (*Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	exe := Resolve(argv[0])
	if exe == "" {
		return nil, fmt.Errorf("command not found: %s", argv[0])
	}
	if ext := strings.ToLower(filepath.Ext(exe)); ext == ".cmd" || ext == ".bat" {
		for _, arg := range argv[1:] {
			if strings.ContainsAny(arg, cmdUnsafe) {
				return nil, fmt.Errorf("refusing to pass %q to %s: %w; send text like this on stdin instead",
					arg, filepath.Base(exe), ErrUnsafeShimArgument)
			}
		}
	}
	cmd, err := buildCommand(context.Background(), ArgsCommand(append([]string{exe}, argv[1:]...)...))
	if err != nil {
		return nil, err
	}
	cmd.Dir = opts.Cwd
	cmd.Env = mergeEnv(opts.Env)
	if opts.Stdin != nil {
		cmd.Stdin = strings.NewReader(*opts.Stdin)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	cmd.WaitDelay = pipeGrace

	group, err := newProcessGroup(cmd)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		group.close()
		return nil, err
	}
	if err := group.started(cmd.Process.Pid); err != nil {
		cmd.Wait()
		group.close()
		return nil, err
	}
	return &Process{cmd: cmd, group: group, stdout: stdout, stderr: stderr, exitCode: -1}, nil
}

// Lines calls fn for every non-empty stdout line (without its line ending) until
// stdout closes or fn returns false.
func (p *Process) Lines(fn func(line string) bool) error {
	sc := bufio.NewScanner(p.stdout)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		line := strings.TrimRight(strings.ToValidUTF8(sc.Text(), "�"), "\r")
		if line == "" {
			continue
		}
		if !fn(line) {
			return nil
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

// Wait waits for exit and returns the exit code (-1 if it never ran normally).
func (p *Process) Wait() int {
	p.waitOnce.Do(func() {
		p.waitErr = p.cmd.Wait()
		p.groupMu.Lock()
		if p.cmd.ProcessState != nil {
			p.exitCode = p.cmd.ProcessState.ExitCode()
		}
		p.exited.Store(true)
		p.group.close() // Windows: KILL_ON_JOB_CLOSE ends anything left in the job
		p.groupMu.Unlock()
	})
	return p.exitCode
}

// Stderr returns everything the process wrote to stderr so far.
func (p *Process) Stderr() string { return p.stderr.String() }

// Pid is the process id of the started command.
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// Kill ends the process and everything it started (including processes it left
// running after exiting, such as `npm run dev &`), then reaps it. Safe to call more
// than once, concurrently with Wait, and after the process has exited.
func (p *Process) Kill() {
	p.killOnce.Do(func() {
		p.groupMu.Lock()
		if !p.exited.Load() {
			p.group.kill()
		}
		p.groupMu.Unlock()
	})
	p.Wait()
	p.group.killLeftovers()
}

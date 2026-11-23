package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// maxCommandBytes bounds an approved command.
	maxCommandBytes = 128 * 1024
	// maxTimeoutSeconds bounds a command deadline.
	maxTimeoutSeconds = 600
	// DefaultOutputLimit is the retained output of one command, in bytes.
	DefaultOutputLimit = 16000
	readChunk          = 4096
)

// Shell runs approved commands in the project (orchestra/tools/shell.py).
//
// The working directory is the project, but a shell is not confined to it:
// approval grants normal OS access. Each command runs in its own process tree
// (a Job Object on Windows, a new session elsewhere), which is killed when the
// command ends, times out or is cancelled.
type Shell struct {
	Root        string
	Timeout     any // default deadline in seconds (a number); nil means 30
	OutputLimit int // retained bytes; 0 means DefaultOutputLimit
}

// NewShell returns a shell tool rooted at root.
func NewShell(root string) *Shell {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Shell{Root: root}
}

// Run executes command, calling onOutput (on the caller's goroutine) with live
// UTF-8 text as it arrives, and returns the bounded output for model history.
// timeout overrides the default deadline: a number of seconds in (0, 600].
// Cancelling ctx kills the command tree and returns ctx.Err().
func (s *Shell) Run(ctx context.Context, command string, timeout any, onOutput func(string)) (Result, error) {
	if strings.TrimSpace(command) == "" {
		return Result{}, toolErr("command must be a non-empty string")
	}
	if len(command) > maxCommandBytes {
		return Result{}, toolErr("Command exceeds 128 KiB limit")
	}
	deadlineValue := timeout
	if deadlineValue == nil {
		deadlineValue = s.Timeout
	}
	if deadlineValue == nil {
		deadlineValue = 30
	}
	seconds, repr, ok := numberArg(deadlineValue)
	if !ok || !(seconds > 0 && seconds <= maxTimeoutSeconds) {
		return Result{}, toolErr("timeout must be greater than zero and at most 600 seconds")
	}
	limit := s.OutputLimit
	if limit <= 0 {
		limit = DefaultOutputLimit
	}
	emit := func(text string) {
		if onOutput != nil && text != "" {
			onOutput(text)
		}
	}

	proc, err := s.start(command)
	if err != nil {
		return Result{}, err
	}
	defer proc.stop()

	timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer timer.Stop()
	var output []byte
	var decoder utf8Decoder
	truncated, timedOut := false, false
read:
	for {
		select {
		case chunk, open := <-proc.chunks:
			if !open {
				break read
			}
			remaining := limit - len(output)
			retained := chunk[:max(0, min(remaining, len(chunk)))]
			output = append(output, retained...)
			if len(retained) > 0 {
				emit(decoder.decode(retained, false))
			}
			truncated = truncated || len(chunk) > remaining
		case <-timer.C:
			timedOut = true
			break read
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if !timedOut {
		select {
		case <-proc.exited:
		case <-timer.C:
			timedOut = true
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	text := decodeReplace(output)
	emit(decoder.decode(nil, true))
	if truncated {
		text += "\n[output truncated]"
		emit("\n[output truncated]")
	}
	if timedOut {
		marker := "\n[command timed out after " + repr + "s]"
		text += marker
		emit(marker)
	}
	return Result{Output: text, OK: !timedOut && proc.exitCode() == 0}, nil
}

// running is one started command with its merged stdout/stderr pipe.
type running struct {
	cmd    *exec.Cmd
	tree   *procTree
	chunks chan []byte
	exited chan struct{}
	done   chan struct{}
}

func (s *Shell) start(command string) (*running, error) {
	cmd := shellCommand(command)
	cmd.Dir = s.Root
	cmd.Env = commandEnv()
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	tree, err := startTree(cmd)
	pw.Close()
	if err != nil {
		pr.Close()
		return nil, err
	}
	r := &running{cmd: cmd, tree: tree, chunks: make(chan []byte, 16),
		exited: make(chan struct{}), done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(r.exited)
	}()
	go func() {
		defer close(r.chunks)
		defer pr.Close()
		buf := make([]byte, readChunk)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				select {
				case r.chunks <- chunk:
				case <-r.done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return r, nil
}

func (r *running) exitCode() int {
	if r.cmd.ProcessState == nil {
		return -1
	}
	return r.cmd.ProcessState.ExitCode()
}

// stop kills everything the command started (even after a normal exit, as
// Python does) and reaps it. The pipe reader ends once the writers are gone.
func (r *running) stop() {
	close(r.done)
	r.tree.kill()
	<-r.exited
	r.tree.close()
}

// commandEnv is the parent environment without secret-suffix variables.
func commandEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key == "" && strings.HasPrefix(kv, "=") { // Windows per-drive cwd entries
			env = append(env, kv)
			continue
		}
		upper := strings.ToUpper(key)
		if secretName(upper) {
			continue
		}
		if upper == "PYTHONIOENCODING" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "PYTHONIOENCODING=utf-8")
}

// secretName reports whether an (upper-case) environment variable name looks like a
// credential, so it is not passed to commands the model runs (security review
// 2026-11-23; wider than Python's _KEY/_TOKEN/_SECRET suffixes).
func secretName(upper string) bool {
	for _, suffix := range []string{"_KEY", "_PAT", "_AUTH", "_CREDENTIAL", "_CREDENTIALS", "_PASSWD"} {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	for _, part := range []string{"TOKEN", "SECRET", "PASSWORD"} {
		if strings.Contains(upper, part) {
			return true
		}
	}
	return false
}

// utf8Decoder is an incremental UTF-8 decoder with replacement, holding back an
// incomplete sequence at the end of a chunk.
type utf8Decoder struct{ pending []byte }

func (d *utf8Decoder) decode(b []byte, final bool) string {
	data := append(d.pending, b...)
	d.pending = nil
	if !final {
		if cut := incompleteTail(data); cut < len(data) {
			d.pending = append([]byte(nil), data[cut:]...)
			data = data[:cut]
		}
	}
	return decodeReplace(data)
}

// incompleteTail returns where a trailing, still-completable sequence starts.
func incompleteTail(b []byte) int {
	for back := 1; back <= 3 && back <= len(b); back++ {
		i := len(b) - back
		if b[i] < 0x80 {
			return len(b)
		}
		if b[i] >= 0xC0 {
			need := seqLen(b[i])
			if need > back && validPrefix(b[i:]) == back {
				return i
			}
			return len(b)
		}
	}
	return len(b)
}

// decodeReplace decodes b, replacing each maximal invalid subpart with U+FFFD
// (Python's errors="replace").
func decodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			sb.WriteRune(r)
			i += size
			continue
		}
		sb.WriteRune(utf8.RuneError)
		i += max(1, validPrefix(b[i:]))
	}
	return sb.String()
}

func seqLen(lead byte) int {
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		return 2
	case lead >= 0xE0 && lead <= 0xEF:
		return 3
	case lead >= 0xF0 && lead <= 0xF4:
		return 4
	}
	return 0
}

// validPrefix is the length of the longest prefix of b that starts a valid
// sequence (lead byte plus allowed continuation bytes), at most the full length.
func validPrefix(b []byte) int {
	need := seqLen(b[0])
	if need == 0 {
		return 1
	}
	n := 1
	for n < need && n < len(b) {
		c := b[n]
		lo, hi := byte(0x80), byte(0xBF)
		if n == 1 {
			switch b[0] {
			case 0xE0:
				lo = 0xA0
			case 0xED:
				hi = 0x9F
			case 0xF0:
				lo = 0x90
			case 0xF4:
				hi = 0x8F
			}
		}
		if c < lo || c > hi {
			break
		}
		n++
	}
	return n
}

// Package chat is `orchestra chat`: talk to any player from the terminal
// (orchestra/chat.py).
//
// One prompt runs one turn and exits. With no prompt it opens a conversation that keeps
// the same backend session until you type /exit. Ctrl+C during a turn stops that turn
// (and kills the CLI's process tree); Ctrl+C at the prompt exits.
package chat

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Adeel0-0/agent-orchestra/internal/players"
	"github.com/Adeel0-0/agent-orchestra/internal/registry"
)

// Options are `orchestra chat`'s arguments, plus hooks tests use to replace the terminal.
type Options struct {
	Agent      string
	Prompt     *string // nil opens a conversation; "-" reads the prompt from stdin
	Cwd        string  // "" means the current folder
	Model      string
	Permission string // "" means "edit"
	Resume     string // backend session id to continue
	JSON       bool   // one JSON event per line; approvals are denied

	// Lookup finds the player; nil means registry.Get. Health checks go through
	// registry.Check, so a test Spec sets Health to skip the executable lookup.
	Lookup func(id string) (registry.Spec, error)
	// Color forces colors on or off; nil detects a terminal and honours NO_COLOR.
	Color *bool
	// Interactive says whether stdin is a terminal that can answer approval prompts;
	// nil detects it. Non-interactive input always denies.
	Interactive *bool
	// Interrupts delivers Ctrl+C; nil installs a handler for os.Interrupt.
	Interrupts <-chan os.Signal
}

// Exit codes, as in Python.
const (
	exitOK          = 0
	exitTurnFailed  = 1
	exitNotReady    = 2
	exitInterrupted = 130
)

// Run runs `orchestra chat` and returns the process exit code.
func Run(opts Options, stdin io.Reader, stdout, stderr io.Writer) int {
	cwd := opts.Cwd
	if cwd == "" {
		cwd = "."
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		fmt.Fprintf(stderr, "[FAILED] Not a folder: %s\n", cwd)
		return exitNotReady
	}
	prompt := opts.Prompt
	if prompt != nil && *prompt == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "[FAILED] %v\n", err)
			return exitNotReady
		}
		text := string(data)
		prompt = &text
	}
	permission := opts.Permission
	if permission == "" {
		permission = "edit"
	}

	lookup := opts.Lookup
	if lookup == nil {
		lookup = registry.Get
	}
	spec, err := lookup(opts.Agent)
	if err != nil {
		fmt.Fprintf(stderr, "[FAILED] %v\n", err)
		return exitNotReady
	}
	// Validate model, resume and project before health checks, with actionable errors.
	player, err := spec.New(players.Options{Cwd: cwd, Model: opts.Model, Permission: permission, SessionID: opts.Resume})
	if err != nil {
		fmt.Fprintf(stderr, "[FAILED] %v\n", err)
		return exitNotReady
	}
	if health := registry.Check(spec); !health.Ready() {
		problem := "not logged in"
		if health.Path == "" {
			problem = "not installed"
		}
		fmt.Fprintf(stderr, "%s is not ready: %s.\n", spec.DisplayName, problem)
		fmt.Fprintf(stderr, "  fix: %s\n", health.Fix)
		return exitNotReady
	}

	s := &session{player: player, asJSON: opts.JSON, out: stdout, lines: newLineReader(stdin)}
	defer s.lines.close()
	if !opts.JSON {
		s.renderer = NewRenderer(stdout, opts.Color, spec.LoginHint, cwd)
	}
	if opts.Interactive != nil {
		s.interactive = *opts.Interactive
	} else {
		f, ok := stdin.(*os.File)
		s.interactive = ok && isTerminal(f.Fd())
	}
	s.interrupts = opts.Interrupts
	if s.interrupts == nil {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		defer signal.Stop(c)
		s.interrupts = c
	}

	if prompt != nil {
		ok, stopped := s.turn(*prompt)
		if id := player.SessionID(); id != "" && !opts.JSON {
			fmt.Fprintf(stderr, "session: %s  (continue with --resume %s)\n", id, id)
		}
		switch {
		case stopped:
			return exitInterrupted
		case ok:
			return exitOK
		}
		return exitTurnFailed
	}
	return s.loop(spec.DisplayName, cwd, permission)
}

type session struct {
	player      players.Player
	renderer    *Renderer // nil in JSON mode
	asJSON      bool
	out         io.Writer
	lines       *lineReader
	interactive bool
	interrupts  <-chan os.Signal
}

// loop is the interactive conversation.
func (s *session) loop(name, cwd, permission string) int {
	fmt.Fprintf(s.out, "%s in %s  [%s]  /exit to quit\n", name, cwd, permission)
	for {
		fmt.Fprint(s.out, "\n> ")
		var line string
		select {
		case l, ok := <-s.lines.next():
			if !ok {
				fmt.Fprintln(s.out)
				return exitOK
			}
			line = strings.TrimSpace(l)
		case <-s.interrupts:
			fmt.Fprintln(s.out)
			return exitOK
		}
		switch line {
		case "":
			continue
		case "/exit", "/quit":
			return exitOK
		case "/session":
			if id := s.player.SessionID(); id != "" {
				fmt.Fprintln(s.out, id)
			} else {
				fmt.Fprintln(s.out, "(none yet)")
			}
			continue
		}
		s.turn(line)
	}
}

// turn runs one turn. ok is false when an error event was seen; stopped is true when the
// user pressed Ctrl+C, which cancels the turn and kills the backend.
func (s *session) turn(prompt string) (ok, stopped bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ok = true
	emit := func(evt players.Event) {
		if evt.Type == players.Error {
			ok = false
		}
		s.display(evt)
		if evt.Type == players.PermissionRequested {
			allow := !s.asJSON && s.askPermission(ctx)
			if err := s.player.AnswerPermission(evt.Str("request_id"), allow); err != nil {
				ok = false
				s.display(players.ErrorEvent(err.Error()))
			}
		}
	}
	done := make(chan error, 1)
	go func() { done <- s.player.Send(ctx, prompt, emit) }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			ok = false
			s.display(players.ErrorEvent(err.Error()))
		}
		return ok, false
	case <-s.interrupts:
		cancel()
		<-done // Send has killed the backend's process tree
		if s.renderer != nil {
			s.renderer.stopped()
		}
		return false, true
	}
}

func (s *session) display(evt players.Event) {
	if s.asJSON {
		fmt.Fprintln(s.out, EventJSON(evt))
	} else if s.renderer != nil {
		s.renderer.Show(evt)
	}
}

// askPermission asks on the terminal. Non-interactive input always denies; only an
// explicit yes grants an action. A cancelled turn denies.
func (s *session) askPermission(ctx context.Context) bool {
	if !s.interactive {
		return false
	}
	fmt.Fprint(s.out, "Allow this action? [y/N] ")
	select {
	case line, ok := <-s.lines.next():
		if !ok {
			return false
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "y" || answer == "yes"
	case <-ctx.Done():
		return false
	}
}

// EventJSON is one --json output line: Python's json.dumps(evt.to_dict(),
// ensure_ascii=False) with "type" first, then the data fields in sorted order.
func EventJSON(evt players.Event) string {
	keys := make([]string, 0, len(evt.Data))
	for k := range evt.Data {
		if k != "type" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(`{"type": ` + players.DumpsJSON(evt.Type))
	for _, k := range keys {
		b.WriteString(", " + players.DumpsJSON(k) + ": " + players.DumpsJSON(evt.Data[k]))
	}
	b.WriteString("}")
	return b.String()
}

// lineReader reads stdin lines on a goroutine so a prompt can also wait for Ctrl+C.
// It starts on first use, so a one-shot turn never reads stdin unless it must.
type lineReader struct {
	r     io.Reader
	ch    chan string
	stop  chan struct{}
	start sync.Once
	end   sync.Once
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{r: r, ch: make(chan string), stop: make(chan struct{})}
}

// next returns the channel of lines; it is closed at end of input.
func (l *lineReader) next() <-chan string {
	l.start.Do(func() { go l.read() })
	return l.ch
}

func (l *lineReader) read() {
	defer close(l.ch)
	br := bufio.NewReader(l.r)
	for {
		s, err := br.ReadString('\n')
		if s != "" {
			s = strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
			select {
			case l.ch <- s:
			case <-l.stop:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// close lets a reader blocked on delivering a line exit. A reader blocked inside Read
// stays until stdin closes, as with any blocking console read.
func (l *lineReader) close() { l.end.Do(func() { close(l.stop) }) }

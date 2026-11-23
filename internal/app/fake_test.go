package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Adeel0-0/agent-orchestra/internal/players"
)

// gate is a resettable asyncio.Event.
type gate struct {
	mu  sync.Mutex
	ch  chan struct{}
	set bool
}

func newGate() *gate { return &gate{ch: make(chan struct{})} }

func (g *gate) Set() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.set {
		g.set = true
		close(g.ch)
	}
}

func (g *gate) Clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.set {
		g.set = false
		g.ch = make(chan struct{})
	}
}

func (g *gate) C() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ch
}

func (g *gate) Wait(t *testing.T) {
	t.Helper()
	select {
	case <-g.C():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for gate")
	}
}

// controlledPlayer is Python's ControlledPlayer: a real in-process producer whose
// lifetime is independent of any subscriber.
type controlledPlayer struct {
	started    *gate
	proceed    *gate
	sessionID  string
	permission bool

	mu            sync.Mutex
	cancelled     bool
	history       []string
	cancelErr     error
	snapshotCalls int
	send          func(ctx context.Context, prompt string, emit func(players.Event)) error
}

func newControlledPlayer() *controlledPlayer {
	return &controlledPlayer{started: newGate(), proceed: newGate(), sessionID: "backend-one"}
}

func (p *controlledPlayer) ID() string        { return "codex" }
func (p *controlledPlayer) SessionID() string { return p.sessionID }

func (p *controlledPlayer) Send(ctx context.Context, prompt string, emit func(players.Event)) error {
	p.mu.Lock()
	custom := p.send
	p.history = append(p.history, prompt)
	p.mu.Unlock()
	if custom != nil {
		return custom(ctx, prompt, emit)
	}
	emit(players.NewEvent(players.SessionStarted, map[string]any{"backend_session_id": p.sessionID, "model": nil}))
	p.started.Set()
	emit(players.NewEvent(players.TextDelta, map[string]any{"text": "first"}))
	if p.permission {
		emit(players.NewEvent(players.PermissionRequested, map[string]any{
			"request_id": "approval", "tool": "run_command", "input": map[string]any{"command": "echo test"}, "reason": "test"}))
	}
	select {
	case <-p.proceed.C():
	case <-ctx.Done():
		p.mu.Lock()
		p.cancelled = true
		p.mu.Unlock()
		return ctx.Err()
	}
	emit(players.NewEvent(players.TextDelta, map[string]any{"text": "last"}))
	emit(players.NewEvent(players.TurnCompleted, map[string]any{"stop_reason": "end_turn"}))
	return nil
}

func (p *controlledPlayer) AnswerPermission(rid string, allow bool) error {
	p.mu.Lock()
	if allow {
		p.history = append(p.history, "allowed")
	} else {
		p.history = append(p.history, "denied")
	}
	p.mu.Unlock()
	p.proceed.Set()
	return nil
}

// Cancel is the optional Canceller cleanup hook.
func (p *controlledPlayer) Cancel() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cancelErr
}

func (p *controlledPlayer) SnapshotState() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshotCalls++
	h := make([]any, len(p.history))
	for i, s := range p.history {
		h[i] = s
	}
	return map[string]any{"history": h}
}

func (p *controlledPlayer) SnapshotRevision() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int64(len(p.history))
}

func (p *controlledPlayer) RestoreState(state map[string]any) error {
	raw, ok := state["history"].([]any)
	if !ok {
		return errors.New("bad state")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.history = p.history[:0]
	for _, v := range raw {
		s, _ := v.(string)
		p.history = append(p.history, s)
	}
	return nil
}

func (p *controlledPlayer) History() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.history...)
}

func (p *controlledPlayer) Cancelled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cancelled
}

// tempLayout makes <tmp>/project and returns (root, project).
func tempLayout(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, project
}

func openTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := OpenStore(dir, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

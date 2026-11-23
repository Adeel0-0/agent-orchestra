// Command uifixture is the offline server the browser tests (web/e2e) run against:
// the real app server and embedded UI with a fake agent and a throwaway project.
// It is never part of the product binary. Started by web/e2e/workspace.spec.ts with
// `go run ./internal/app/uifixture`; it prints one ORCHESTRA_E2E_READY line with the
// bootstrap URL and stops when a line arrives on stdin.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/Adeel0-0/agent-orchestra/internal/app"
	"github.com/Adeel0-0/agent-orchestra/internal/players"
	"github.com/Adeel0-0/agent-orchestra/internal/webui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "uifixture:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := os.MkdirTemp("", "orchestra-ui-test-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	project := filepath.Join(root, "Sample project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(project, "greeting.py"), []byte("print(\"hello\")\n"), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init"}, {"add", "greeting.py"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", project}, args...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %v: %s", args, err, out)
		}
	}

	store, err := app.OpenStore(filepath.Join(root, "data"), app.StoreOptions{})
	if err != nil {
		return err
	}
	listener, err := app.ReserveListener()
	if err != nil {
		return err
	}
	port := listener.Addr().(*net.TCPAddr).Port

	ready := true
	notReady := false
	codexPath, fixture, fix := "fixture", "fixture", "Configure a key privately in a terminal"
	a, err := app.New(app.Config{
		Store: store, Port: port, Web: webui.FS(),
		Factory: func(s app.Session, cwd string) (players.Player, error) {
			id := "offline-test"
			if s.BackendSessionID != nil {
				id = *s.BackendSessionID
			}
			return &fakePlayer{sessionID: id, cwd: cwd}, nil
		},
		PlayerHealth: func(context.Context) []app.PlayerHealth {
			return []app.PlayerHealth{
				{ID: "codex", Name: "Codex (offline test)", Path: &codexPath, Version: &fixture, LoggedIn: &ready},
				{ID: "openrouter", Name: "OpenRouter", LoggedIn: &notReady, Fix: &fix},
			}
		},
		Models: func(context.Context) ([]any, error) {
			return []any{
				map[string]any{"id": "fixture-small", "name": "Small offline model"},
				map[string]any{"id": "fixture-large", "name": "Large offline model"},
			}, nil
		},
		KeyPresent:   func() bool { return false },
		FolderPicker: func(context.Context) (string, error) { return project, nil },
	})
	if err != nil {
		return err
	}
	server := &http.Server{Handler: a, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	line, _ := json.Marshal(map[string]string{
		"url": fmt.Sprintf("http://127.0.0.1:%d/#bootstrap=%s", port, a.Bootstrap.Issue(60*time.Second)),
	})
	fmt.Printf("ORCHESTRA_E2E_READY %s\n", line)

	bufio.NewReader(os.Stdin).ReadString('\n') // the test writes a line (or closes stdin) to stop us
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.Close()
	server.Shutdown(ctx)
	<-served
	return store.Close()
}

// fakePlayer replays one scripted turn that asks permission to edit greeting.py.
type fakePlayer struct {
	sessionID string
	cwd       string

	mu      sync.Mutex
	pending chan bool
}

func (p *fakePlayer) ID() string        { return "codex" }
func (p *fakePlayer) SessionID() string { return p.sessionID }

func (p *fakePlayer) AnswerPermission(requestID string, allow bool) error {
	p.mu.Lock()
	ch := p.pending
	p.mu.Unlock()
	if ch == nil || requestID != "edit-request" {
		return fmt.Errorf("permission request is not pending")
	}
	select {
	case ch <- allow:
	default:
	}
	return nil
}

func (p *fakePlayer) Send(ctx context.Context, _ string, emit func(players.Event)) error {
	edit := map[string]any{"path": "greeting.py", "old": "hello", "new": "goodbye"}
	emit(players.NewEvent(players.SessionStarted, map[string]any{"backend_session_id": p.sessionID, "model": "fixture-small"}))
	emit(players.NewEvent(players.TextDelta, map[string]any{"text": "This is an **offline acceptance test**. I will update `greeting.py` after your approval."}))
	emit(players.NewEvent(players.ToolStarted, map[string]any{"id": "edit-1", "name": "edit_file", "input": edit}))
	ch := make(chan bool, 1)
	p.mu.Lock()
	p.pending = ch
	p.mu.Unlock()
	emit(players.NewEvent(players.PermissionRequested, map[string]any{
		"request_id": "edit-request", "tool": "edit_file", "input": edit, "reason": "Approve this test edit.",
	}))
	var allow bool
	select {
	case allow = <-ch:
	case <-ctx.Done():
		return ctx.Err()
	}
	if allow {
		if err := os.WriteFile(filepath.Join(p.cwd, "greeting.py"), []byte("print(\"goodbye\")\n"), 0o644); err != nil {
			return err
		}
		emit(players.NewEvent(players.FileChanged, map[string]any{"path": "greeting.py", "kind": "update"}))
	}
	output, text := "Edit denied", "No files changed."
	if allow {
		output, text = "Updated greeting.py", "The file now prints `goodbye`."
	}
	emit(players.NewEvent(players.ToolOutput, map[string]any{"id": "edit-1", "chunk": output}))
	emit(players.NewEvent(players.ToolFinished, map[string]any{"id": "edit-1", "ok": allow}))
	emit(players.NewEvent(players.TextDelta, map[string]any{"text": text}))
	emit(players.NewEvent(players.Usage, map[string]any{"input_tokens": 120, "output_tokens": 32, "cost_usd": 0}))
	emit(players.NewEvent(players.TurnCompleted, map[string]any{"stop_reason": "end_turn"}))
	return nil
}

package players

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake agent CLI (tests/fake_cli.py):
//
//	replay <fixture.jsonl> [exit_code]   print the fixture's lines, then exit
//	echo                                 print stdin back as a Codex agent message
//	hang <marker>                        start a grandchild that writes marker if it
//	                                     outlives the cancel, print one event, sleep
const helperEnv = "ORCHESTRA_PLAYERS_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "grandchild":
		time.Sleep(3 * time.Second)
		os.WriteFile(os.Getenv("MARKER"), []byte("alive"), 0o644)
		os.Exit(0)
	default:
		os.Exit(fakeCLI(helperArgs()))
	}
}

// helperArgs are the arguments after "--".
func helperArgs() []string {
	for i, a := range os.Args {
		if a == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

func fakeCLI(args []string) int {
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "replay":
		data, err := os.ReadFile(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		os.Stdout.Write(data)
		fmt.Fprintln(os.Stderr, "fake stderr line")
		if len(args) > 2 {
			code, _ := strconv.Atoi(args[2])
			return code
		}
		return 0
	case "echo":
		data, _ := io.ReadAll(os.Stdin)
		line, _ := json.Marshal(map[string]any{"type": "item.completed",
			"item": map[string]any{"type": "agent_message", "text": string(data)}})
		fmt.Println(string(line))
		return 0
	case "hang":
		gc := exec.Command(os.Args[0], "-test.run=NONE")
		gc.Env = append(os.Environ(), helperEnv+"=grandchild", "MARKER="+args[1])
		if err := gc.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "grandchild failed:", err)
			return 2
		}
		fmt.Println(`{"type":"thread.started","thread_id":"hang"}`)
		time.Sleep(30 * time.Second)
		return 0
	}
	return 2
}

// fakeBackend is Codex's mapper with the test binary as the CLI.
type fakeBackend struct {
	args  []string
	stdin *string
}

func (b fakeBackend) BuildCommand(string, Options) ([]string, *string) {
	return append([]string{os.Args[0], "-test.run=NONE", "--"}, b.args...), b.stdin
}

func (fakeBackend) NewMapper(cwd string) Mapper { return NewCodexMapper(cwd) }

func runFake(t *testing.T, backend CLIBackend) (*CLIPlayer, []Event) {
	t.Helper()
	t.Setenv(helperEnv, "cli")
	p, err := NewCLIPlayer("fake", backend, Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	if err := p.Send(context.Background(), "hi", func(e Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	return p, events
}

func absFixture(t *testing.T, name string) string {
	abs, err := filepath.Abs(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestPlayerFullTurnAndSessionID(t *testing.T) {
	p, ev := runFake(t, fakeBackend{args: []string{"replay", absFixture(t, "codex_edit.jsonl")}})
	if p.SessionID() != "01a1d9a8-e58b-7e22-8a89-cbe634014e35" {
		t.Fatalf("session %q", p.SessionID())
	}
	if count(ev, TurnCompleted) != 1 || ev[len(ev)-1].Str("stop_reason") != "end_turn" {
		t.Fatalf("events %v", types(ev))
	}
}

func TestPlayerCrashWithoutTurnEndStillCompletes(t *testing.T) {
	partial := filepath.Join(t.TempDir(), "partial.jsonl")
	os.WriteFile(partial, []byte(`{"type":"thread.started","thread_id":"t"}`+"\n"), 0o644)
	_, ev := runFake(t, fakeBackend{args: []string{"replay", partial, "3"}})
	if got := types(ev); len(got) < 2 || got[len(got)-2] != Error || got[len(got)-1] != TurnCompleted {
		t.Fatalf("events %v", got)
	}
	if !strings.Contains(ev[len(ev)-2].Str("message"), "fake stderr line") {
		t.Fatalf("error %v", ev[len(ev)-2].Data)
	}
	if ev[len(ev)-1].Str("stop_reason") != "error" {
		t.Fatalf("last %v", ev[len(ev)-1].Data)
	}
}

func TestPlayerStdinRoundTripWithShellCharacters(t *testing.T) {
	text := `a & b | c %PATH% "q"`
	_, ev := runFake(t, fakeBackend{args: []string{"echo"}, stdin: &text})
	if got := textOf(ev); got != text {
		t.Fatalf("stdin came back as %q", got)
	}
}

func TestPlayerMissingCommandCompletesWithError(t *testing.T) {
	p, err := NewCLIPlayer("missing", missingBackend{}, Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var ev []Event
	if err := p.Send(context.Background(), "hi", func(e Event) { ev = append(ev, e) }); err != nil {
		t.Fatal(err)
	}
	if got := types(ev); len(got) != 2 || got[0] != Error || got[1] != TurnCompleted || ev[1].Str("stop_reason") != "error" {
		t.Fatalf("events %v", ev)
	}
}

type missingBackend struct{}

func (missingBackend) BuildCommand(string, Options) ([]string, *string) {
	return []string{"definitely-not-a-real-cli-xyz"}, nil
}
func (missingBackend) NewMapper(cwd string) Mapper { return NewCodexMapper(cwd) }

func TestPlayerCancelKillsTheWholeTree(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "grandchild_survived")
	t.Setenv(helperEnv, "cli")
	p, err := NewCLIPlayer("fake", fakeBackend{args: []string{"hang", marker}}, Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- p.Send(ctx, "hi", func(e Event) {
			if e.Type == SessionStarted {
				close(started)
			}
		})
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("fake CLI never started")
	}
	t0 := time.Now()
	cancel() // what Ctrl+C does
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Send returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Send did not return after cancel")
	}
	if time.Since(t0) > 10*time.Second {
		t.Fatalf("cancel took %v", time.Since(t0))
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("grandchild survived the cancel")
	}
}

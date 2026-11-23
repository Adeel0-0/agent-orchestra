package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStreamReadsLinesAndStdin(t *testing.T) {
	stdin := "hello & | % \"q\""
	p, err := StartStream([]string{os.Args[0], "-test.run=NONE"}, StreamOptions{
		Env: map[string]string{helperEnv: "echo-stdin"}, Stdin: &stdin,
	})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := p.Lines(func(l string) bool { lines = append(lines, l); return true }); err != nil {
		t.Fatal(err)
	}
	if code := p.Wait(); code != 0 || len(lines) != 2 || lines[0] != "line one" || lines[1] != "stdin="+stdin {
		t.Fatalf("code %d lines %q stderr %q", code, lines, p.Stderr())
	}
}

func TestStreamKillEndsTheWholeTree(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "grandchild_survived")
	p, err := StartStream([]string{os.Args[0], "-test.run=NONE"}, StreamOptions{
		Env: map[string]string{helperEnv: "child", "MARKER": marker},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go p.Lines(func(l string) bool { got <- l; return false })
	select {
	case l := <-got:
		if !strings.Contains(l, "partial output") {
			t.Fatalf("first line %q", l)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no output")
	}
	start := time.Now()
	p.Kill()
	if time.Since(start) > 8*time.Second {
		t.Fatalf("kill took %v", time.Since(start))
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("grandchild survived Kill")
	}
	p.Kill() // idempotent
}

func TestStreamRefusesUnsafeArgumentsToCmdShims(t *testing.T) {
	if os.PathSeparator != '\\' {
		t.Skip("cmd.exe shims are Windows-only")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "tool.cmd")
	os.WriteFile(shim, []byte("@echo off\r\necho ok\r\n"), 0o644)
	if _, err := StartStream([]string{shim, "a&calc"}, StreamOptions{}); err == nil || !strings.Contains(err.Error(), "cmd.exe") {
		t.Fatalf("unsafe argument accepted: %v", err)
	}
	p, err := StartStream([]string{shim, "--json", "sandbox_mode=read-only"}, StreamOptions{})
	if err != nil {
		t.Fatalf("safe arguments refused: %v", err)
	}
	p.Wait()
}

func TestStreamMissingCommand(t *testing.T) {
	if _, err := StartStream([]string{"definitely-not-a-real-cli-xyz"}, StreamOptions{}); err == nil {
		t.Fatal("missing command started")
	}
}

func TestKillAfterNormalExitEndsLeftoverProcesses(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "leftover_survived")
	p, err := StartStream([]string{os.Args[0], "-test.run=NONE"}, StreamOptions{
		Env: map[string]string{helperEnv: "spawn-exit", "MARKER": marker},
	})
	if err != nil {
		t.Fatal(err)
	}
	p.Lines(func(string) bool { return true })
	if code := p.Wait(); code != 0 {
		t.Fatalf("exit %d: %s", code, p.Stderr())
	}
	p.Kill() // what CLIPlayer.Send does after a normal turn
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a process the command left running survived the end of the turn")
	}
}

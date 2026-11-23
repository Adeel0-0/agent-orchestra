package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const helperArg = "orchestra-tools-helper"

// TestMain lets the test binary act as the child program of shell tests:
// `<test binary> orchestra-tools-helper <mode>`.
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == helperArg {
		os.Exit(runHelper(os.Args[2], os.Args[3:]))
	}
	os.Exit(m.Run())
}

func runHelper(mode string, rest []string) int {
	switch mode {
	case "print":
		fmt.Print(strings.Repeat("x", 10000) + "\n")
	case "sleep":
		time.Sleep(30 * time.Second)
	case "chatty":
		for i := 0; i < 1000; i++ {
			fmt.Println("line")
			time.Sleep(2 * time.Millisecond)
		}
	case "started":
		fmt.Println("started")
		time.Sleep(5 * time.Second)
		_ = os.WriteFile("finished", []byte("done"), 0o644)
	case "env":
		out := map[string]any{}
		for _, k := range []string{"REVIEW_TEST_TOKEN", "REVIEW_TEST_KEY", "REVIEW_TEST_SECRET", "REVIEW_PUBLIC"} {
			if v, ok := os.LookupEnv(k); ok {
				out[k] = v
			} else {
				out[k] = nil
			}
		}
		b, _ := json.Marshal(out)
		fmt.Println(string(b))
	case "launch":
		exe, _ := os.Executable()
		child := exec.Command(exe, helperArg, "sleep")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			return 3
		}
		_ = os.WriteFile("child.pid", []byte(strconv.Itoa(child.Process.Pid)), 0o644)
	case "exit":
		code, _ := strconv.Atoi(rest[0])
		return code
	}
	return 0
}

// helperCommand is a shell command running the test binary in helper mode.
func helperCommand(t *testing.T, mode string, extra ...string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := fmt.Sprintf(`"%s" %s %s`, exe, helperArg, mode)
	if len(extra) > 0 {
		cmd += " " + strings.Join(extra, " ")
	}
	if runtime.GOOS == "windows" {
		return "& " + cmd
	}
	return cmd
}

func newShell(t *testing.T) *Shell {
	t.Helper()
	root, err := realpath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewShell(root)
}

func echoCommand(text string) string {
	if runtime.GOOS == "windows" {
		return "Write-Output '" + text + "'"
	}
	return "echo " + text
}

func TestCommandTimeoutOverrideIsBounded(t *testing.T) {
	sh := newShell(t)
	sh.Timeout = 0.01
	r, err := sh.Run(context.Background(), echoCommand("extended"), json.Number("10"), nil)
	if err != nil || !r.OK {
		t.Fatalf("run: %+v %v", r, err)
	}
	for _, bad := range []any{0, -1, 601, true, "60", json.Number("0"), json.Number("600.5")} {
		if _, err := sh.Run(context.Background(), echoCommand("bad"), bad, nil); err == nil {
			t.Errorf("timeout %v accepted", bad)
		}
	}
	if _, err := sh.Run(context.Background(), "   ", nil, nil); err == nil {
		t.Error("blank command accepted")
	}
	if _, err := sh.Run(context.Background(), strings.Repeat("x", maxCommandBytes+1), nil, nil); err == nil {
		t.Error("oversize command accepted")
	}
}

func TestUnicodeOutputIsUTF8(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell output encoding")
	}
	r, err := newShell(t).Run(context.Background(), "Write-Output 'café ✓'", nil, nil)
	if err != nil || !r.OK {
		t.Fatalf("run: %+v %v", r, err)
	}
	if !strings.Contains(r.Output, "café ✓") {
		t.Fatalf("output %q", r.Output)
	}
}

func TestShellEnvironmentExcludesSecretSuffixes(t *testing.T) {
	t.Setenv("REVIEW_TEST_TOKEN", "do-not-inherit")
	t.Setenv("REVIEW_TEST_KEY", "do-not-inherit")
	t.Setenv("REVIEW_TEST_SECRET", "do-not-inherit")
	t.Setenv("REVIEW_PUBLIC", "visible")
	r, err := newShell(t).Run(context.Background(), helperCommand(t, "env"), nil, nil)
	if err != nil || !r.OK {
		t.Fatalf("run: %+v %v", r, err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.Output)), &got); err != nil {
		t.Fatalf("output %q: %v", r.Output, err)
	}
	want := map[string]any{"REVIEW_TEST_TOKEN": nil, "REVIEW_TEST_KEY": nil, "REVIEW_TEST_SECRET": nil, "REVIEW_PUBLIC": "visible"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestCancelDoesNotDeadlockWhileOutputIsStreaming(t *testing.T) {
	sh := newShell(t)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		var once bool
		_, err := sh.Run(ctx, helperCommand(t, "chatty"), nil, func(string) {
			if !once {
				once = true
				close(first)
			}
			time.Sleep(5 * time.Millisecond) // a slow consumer
		})
		done <- err
	}()
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("no output")
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel deadlocked")
	}
}

func TestOutputStreamsBeforeCommandFinishesAndCancelKills(t *testing.T) {
	sh := newShell(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		_, err := sh.Run(ctx, helperCommand(t, "started"), nil, func(s string) {
			select {
			case first <- s:
			default:
			}
		})
		done <- err
	}()
	select {
	case s := <-first:
		if !strings.Contains(s, "started") {
			t.Fatalf("first chunk %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no live output")
	}
	if _, err := os.Stat(filepath.Join(sh.Root, "finished")); err == nil {
		t.Fatal("command finished before cancel")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not return")
	}
	time.Sleep(5500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(sh.Root, "finished")); err == nil {
		t.Fatal("cancelled command kept running")
	}
}

func TestCommandRunsInProjectAndCapsOutput(t *testing.T) {
	sh := newShell(t)
	sh.OutputLimit = 200
	r, err := sh.Run(context.Background(), helperCommand(t, "print"), nil, nil)
	if err != nil || !r.OK {
		t.Fatalf("run: %+v %v", r, err)
	}
	if len(r.Output) >= 300 || !strings.Contains(r.Output, "truncated") {
		t.Fatalf("output %d %q", len(r.Output), r.Output)
	}
	pwd := "pwd"
	if runtime.GOOS == "windows" {
		pwd = "(Get-Location).Path"
	}
	r, err = sh.Run(context.Background(), pwd, nil, nil)
	if err != nil || !strings.EqualFold(strings.TrimSpace(r.Output), sh.Root) {
		t.Fatalf("cwd %q, want %q (%v)", r.Output, sh.Root, err)
	}
}

func TestNonZeroExitIsFailure(t *testing.T) {
	r, err := newShell(t).Run(context.Background(), helperCommand(t, "exit", "3"), nil, nil)
	if err != nil || r.OK {
		t.Fatalf("run: %+v %v", r, err)
	}
}

func TestTimeoutReturnsFailure(t *testing.T) {
	sh := newShell(t)
	sh.Timeout = 0.2
	start := time.Now()
	r, err := sh.Run(context.Background(), helperCommand(t, "sleep"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK || !strings.Contains(r.Output, "[command timed out after 0.2s]") {
		t.Fatalf("result %+v", r)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout not enforced")
	}
}

func TestTimeoutKillsDescendantAfterParentHasExited(t *testing.T) {
	sh := newShell(t)
	sh.Timeout = 2
	r, err := sh.Run(context.Background(), helperCommand(t, "launch"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatalf("result %+v", r)
	}
	raw, err := os.ReadFile(filepath.Join(sh.Root, "child.pid"))
	if err != nil {
		t.Fatalf("child.pid: %v (%s)", err, r.Output)
	}
	pid, _ := strconv.Atoi(string(raw))
	defer killTestPID(pid)
	time.Sleep(200 * time.Millisecond)
	if processAlive(pid) {
		t.Fatal("descendant survived command timeout")
	}
}

func TestMaximalSubpartReplacement(t *testing.T) {
	if got := decodeReplace([]byte("a\xe2\x82b")); got != "a�b" {
		t.Fatalf("%q", got)
	}
	var d utf8Decoder
	if got := d.decode([]byte("x\xc3"), false) + d.decode([]byte("\xa9"), false); got != "xé" {
		t.Fatalf("incremental %q", got)
	}
	if got := d.decode([]byte("\xc3"), false) + d.decode(nil, true); got != "�" {
		t.Fatalf("final %q", got)
	}
}

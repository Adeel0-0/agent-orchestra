package runner

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Helper-process pattern: the test binary plays the child and grandchild of a
// shell command when this env var is set.
const helperEnv = "ORCHESTRA_RUNNER_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "child":
		// Start a grandchild that inherits our stdout, print, then hang.
		gc := exec.Command(os.Args[0], "-test.run=NONE")
		gc.Env = append(os.Environ(), helperEnv+"=grandchild")
		gc.Stdout, gc.Stderr = os.Stdout, os.Stderr
		if err := gc.Start(); err != nil {
			fmt.Println("grandchild failed:", err)
			os.Exit(2)
		}
		fmt.Println("partial output")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "spawn-exit":
		// Leave a background process running (like `npm run dev &`), then exit normally.
		gc := exec.Command(os.Args[0], "-test.run=NONE")
		gc.Env = append(os.Environ(), helperEnv+"=grandchild")
		if err := gc.Start(); err != nil {
			os.Exit(2)
		}
		fmt.Println("left a background process")
		os.Exit(0)
	case "grandchild":
		// Leave a marker if still alive 3s later.
		time.Sleep(3 * time.Second)
		os.WriteFile(os.Getenv("MARKER"), []byte("alive"), 0o644)
		os.Exit(0)
	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "json":
		fmt.Println("noise line")
		fmt.Println(`{"tests": 3, "ok": true}`)
		os.Exit(0)
	case "echo-stdin":
		data, _ := io.ReadAll(os.Stdin)
		fmt.Println("line one")
		fmt.Println("stdin=" + string(data))
		os.Exit(0)
	case "exit3":
		fmt.Fprint(os.Stderr, "bad")
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func quoted(path string) string { return `"` + path + `"` }

func TestShellStringCommand(t *testing.T) {
	res := New(t.TempDir()).Run(ShellCommand("echo hello"), Options{})
	if !res.Success || res.ExitCode != 0 || !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("%+v", res)
	}
	if res.Command != "echo hello" {
		t.Fatalf("command display %q", res.Command)
	}
}

func TestShellStringWithSeveralQuotedParts(t *testing.T) {
	// Pipeline commands look like `"C:\...\python.exe" -c "..."`; cmd /c "<cmd>" must keep both quoted parts.
	res := New("").Run(ShellCommand(quoted(os.Args[0])+` "-test.run=NONE"`), Options{Env: map[string]string{helperEnv: "exit3"}})
	if res.ExitCode != 3 {
		t.Fatalf("quoted shell command did not run the helper: %+v", res)
	}
}

func TestArgsCommandExitCodeAndStderr(t *testing.T) {
	res := New("").Run(ArgsCommand(os.Args[0], "-test.run=NONE"), Options{Env: map[string]string{helperEnv: "exit3"}})
	if res.Success || res.ExitCode != 3 || res.Stderr != "bad" {
		t.Fatalf("%+v", res)
	}
}

func TestParsedJSONTakesLastObjectLine(t *testing.T) {
	res := New("").Run(ArgsCommand(os.Args[0], "-test.run=NONE"), Options{Env: map[string]string{helperEnv: "json"}})
	if res.ParsedJSON["tests"] != 3.0 || res.ParsedJSON["ok"] != true {
		t.Fatalf("parsed_json %v from %q", res.ParsedJSON, res.Stdout)
	}
	if obj := ExtractJSON(`  {"whole": [1, 2]}  `); obj["whole"] == nil {
		t.Fatalf("whole-output object not parsed: %v", obj)
	}
	if ExtractJSON("[1, 2]") != nil || ExtractJSON("") != nil {
		t.Fatal("non-object output must not parse")
	}
}

func TestTimeoutResult(t *testing.T) {
	start := time.Now()
	res := New("").Run(ArgsCommand(os.Args[0], "-test.run=NONE"),
		Options{Env: map[string]string{helperEnv: "sleep"}, Timeout: time.Second})
	if res.Success || res.ExitCode != -1 || res.Stderr != "Command timed out after 1 seconds." {
		t.Fatalf("%+v", res)
	}
	if time.Since(start) > 6*time.Second {
		t.Fatalf("timeout took %v", time.Since(start))
	}
	if res.DurationSeconds < 1 || res.DurationSeconds > 6 {
		t.Fatalf("duration %v", res.DurationSeconds)
	}
}

func TestStartFailureIsAResultNotAPanic(t *testing.T) {
	res := New("").Run(ArgsCommand("definitely-not-a-real-binary-xyz"), Options{})
	if res.Success || res.ExitCode != -1 || res.Stderr == "" {
		t.Fatalf("%+v", res)
	}
}

func TestTimeoutKillsGrandchildrenOfAShellCommand(t *testing.T) {
	tmp := t.TempDir()
	marker := filepath.Join(tmp, "grandchild_survived")
	// A shell string, like pipeline commands ("npm run handoff"): shell -> child -> grandchild.
	command := quoted(os.Args[0]) + " -test.run=NONE"

	started := time.Now()
	res := New(tmp).Run(ShellCommand(command), Options{
		Timeout: time.Second,
		Env:     map[string]string{helperEnv: "child", "MARKER": marker},
	})
	elapsed := time.Since(started)

	if res.Success || res.ExitCode != -1 {
		t.Fatalf("expected a timeout: %+v", res)
	}
	if elapsed >= 8*time.Second {
		t.Fatalf("timeout waited %v for a grandchild", elapsed)
	}
	if !strings.Contains(res.Stdout, "partial output") {
		t.Fatalf("partial stdout lost: %q", res.Stdout)
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("grandchild kept running after the timeout")
	}
}

func TestPayloadKeepsTheLast64KB(t *testing.T) {
	big := strings.Repeat("a", 70*1024) + "é" + "END"
	p := Result{Stdout: big, Stderr: "short"}.Payload()
	out := p["stdout"].(string)
	if len(out) > payloadTailBytes || !strings.HasSuffix(out, "éEND") || p["stderr"] != "short" {
		t.Fatalf("len=%d suffix=%q", len(out), out[len(out)-6:])
	}
	if p["parsed_json"] != nil {
		t.Fatalf("parsed_json should be null, got %v", p["parsed_json"])
	}
}

func TestRunScriptUsesPythonForPyFiles(t *testing.T) {
	if _, err := findPython(); err != nil {
		t.Skip("no python on PATH")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.py"), []byte("import sys; print('py', sys.argv[1])\n"), 0o644)
	res := New(dir).RunScript("hello.py", []string{"arg1"}, Options{})
	if !res.Success || strings.TrimSpace(res.Stdout) != "py arg1" {
		t.Fatalf("%+v (os %s)", res, runtime.GOOS)
	}
}

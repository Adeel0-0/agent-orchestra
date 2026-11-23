package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pythonStore returns a function that runs Python code against the real Python Store
// (orchestra/app/store.py) with the repository on PYTHONPATH, or skips.
func pythonStore(t *testing.T) func(code string, args ...string) *exec.Cmd {
	t.Helper()
	python := ""
	for _, name := range []string{"python", "python3"} {
		if p, err := exec.LookPath(name); err == nil {
			python = p
			break
		}
	}
	if python == "" {
		t.Skip("python is not on PATH")
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	command := func(code string, args ...string) *exec.Cmd {
		cmd := exec.Command(python, append([]string{"-c", code}, args...)...)
		cmd.Env = append(os.Environ(), "PYTHONPATH="+repo, "PYTHONIOENCODING=utf-8")
		cmd.Dir = repo
		return cmd
	}
	if out, err := command("import orchestra.app.store").CombinedOutput(); err != nil {
		t.Skipf("the Python Store cannot be imported: %v\n%s", err, out)
	}
	return command
}

const pyCreate = `
import json, sys
from orchestra.app.store import Store
s = Store(sys.argv[1])
p = s.add_project(sys.argv[2])
x = s.create_session(p['id'], 'claude', None, 'edit', title='From Python')
s.append(x['id'], {'type': 'message.user', 'text': 'héllo ☃'})
s.append(x['id'], {'type': 'session.started', 'backend_session_id': 'py-backend', 'model': None}, {'history': ['x']})
s.append(x['id'], {'type': 'turn.completed', 'stop_reason': 'end_turn'})
s.set_settings({'default_player': 'claude'})
s.close()
print(json.dumps({'pid': p['id'], 'sid': x['id'], 'path': p['path']}))
`

const pyRead = `
import json, sys
from orchestra.app.store import Store
s = Store(sys.argv[1])
x = s.session(sys.argv[2])
print(json.dumps({'status': x['status'], 'types': [e['type'] for e in s.events(x['id'])],
                  'projects': len(s.projects()), 'state': s.player_state(x['id'])}))
s.close()
`

const pyHold = `
import sys
from orchestra.app.store import Store
s = Store(sys.argv[1])
print('locked', flush=True)
sys.stdin.readline()
s.close()
`

const pyTry = `
import sys
from orchestra.app.store import Store
try:
    Store(sys.argv[1]).close()
    print('opened')
except RuntimeError as exc:
    print('refused:', exc)
`

func TestPythonCreatedDatabaseIsReadByGoAndBack(t *testing.T) {
	py := pythonStore(t)
	root, project := tempLayout(t)
	data := filepath.Join(root, "data")
	out, err := py(pyCreate, data, project).Output()
	if err != nil {
		t.Fatalf("python create: %v", err)
	}
	var ids struct{ Pid, Sid, Path string }
	if err := json.Unmarshal(out, &ids); err != nil {
		t.Fatalf("python output %q", out)
	}
	transcript := filepath.Join(data, "sessions", ids.Sid+".jsonl")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	os.Chtimes(transcript, old, old)

	store := openTestStore(t, data)
	projects := must[[]Project](t)(store.Projects())
	if len(projects) != 1 || projects[0].ID != ids.Pid || projects[0].Path != ids.Path || projects[0].Name != "project" {
		t.Fatalf("projects = %+v, python path %q", projects, ids.Path)
	}
	// The same folder added from Go maps to the Python row (same normalization).
	if p := must[Project](t)(store.AddProject(project)); p.ID != ids.Pid {
		t.Fatalf("Go AddProject made a new project: %+v vs python path %q", p, ids.Path)
	}
	s := must[Session](t)(store.Session(ids.Sid))
	if s.Title != "From Python" || s.Status != "idle" || s.Player != "claude" || s.BackendSessionID == nil || *s.BackendSessionID != "py-backend" {
		t.Fatalf("session = %+v", s)
	}
	events := must[[]Record](t)(store.Events(ids.Sid, 0, 1000))
	if len(events) != 3 || events[0].Fields()["text"] != "héllo ☃" || events[2].Type != "turn.completed" {
		t.Fatalf("events = %v", events)
	}
	if state := must[map[string]any](t)(store.PlayerState(ids.Sid)); state == nil {
		t.Fatal("no player state")
	}
	if v := must[orderedObject](t)(store.Settings()).Get("default_player"); v != "claude" {
		t.Fatalf("settings default_player = %v", v)
	}
	if info, err := os.Stat(transcript); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("Go rewrote a complete Python transcript (%v)", err)
	}
	must[Record](t)(store.Append(ids.Sid, map[string]any{"type": "text.delta", "text": "from go ü"}, map[string]any{"history": []any{"x", "y"}}))
	store.Close()

	// Python reads the Go additions and keeps the Go mirror (same byte length).
	stamp := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	os.Chtimes(transcript, stamp, stamp)
	out, err = py(pyRead, data, ids.Sid).Output()
	if err != nil {
		t.Fatalf("python read: %v", err)
	}
	var back struct {
		Status   string
		Types    []string
		Projects int
		State    map[string]any
	}
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("python output %q", out)
	}
	if back.Status != "idle" || strings.Join(back.Types, ",") != "message.user,session.started,turn.completed,text.delta" ||
		back.Projects != 1 || len(back.State["history"].([]any)) != 2 {
		t.Fatalf("python sees %+v", back)
	}
	if info, err := os.Stat(transcript); err != nil || !info.ModTime().Equal(stamp) {
		t.Fatalf("Python rewrote the Go-appended transcript (%v)", err)
	}
}

func TestPythonAndGoServersExcludeEachOther(t *testing.T) {
	py := pythonStore(t)
	data := filepath.Join(t.TempDir(), "data")

	// Go holds the directory: Python refuses.
	store := openTestStore(t, data)
	out, err := py(pyTry, data).Output()
	if err != nil || !strings.HasPrefix(string(out), "refused: Another Orchestra server owns this data directory.") {
		t.Fatalf("python while Go holds: %q %v", out, err)
	}
	store.Close()

	// Python holds the directory: Go refuses, then succeeds once Python exits.
	cmd := py(pyHold, data)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("python holder: %q %v", line, err)
	}
	if _, err := OpenStore(data, StoreOptions{}); !errors.Is(err, ErrServerOwned) {
		t.Fatalf("Go while Python holds: %v", err)
	}
	stdin.Write([]byte("\n"))
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	openTestStore(t, data).Close()
	out, err = py(pyTry, data).Output()
	if err != nil || strings.TrimSpace(string(out)) != "opened" {
		t.Fatalf("python after Go closed: %q %v", out, err)
	}
}

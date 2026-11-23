package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type storeFixture struct {
	t       *testing.T
	root    string
	project string
	data    string
	store   *Store
}

func newStoreFixture(t *testing.T) *storeFixture {
	root, project := tempLayout(t)
	f := &storeFixture{t: t, root: root, project: project, data: filepath.Join(root, "data")}
	f.store = openTestStore(t, f.data)
	t.Cleanup(func() { f.store.Close() })
	return f
}

func (f *storeFixture) reopen() {
	f.t.Helper()
	f.store.Close()
	f.store = openTestStore(f.t, f.data)
}

func (f *storeFixture) session() Session {
	f.t.Helper()
	p := must[Project](f.t)(f.store.AddProject(f.project))
	return must[Session](f.t)(f.store.CreateSession(p.ID, "codex", nil, "edit", nil, nil))
}

func TestStoreProjectDedupAndSessionSurviveReopen(t *testing.T) {
	f := newStoreFixture(t)
	s := f.session()
	if n := len(must[[]Project](t)(f.store.Projects())); n != 1 {
		t.Fatalf("projects = %d", n)
	}
	must[Project](t)(f.store.AddProject(f.project + string(filepath.Separator) + "."))
	if strings.Contains(f.project, `\`) { // Windows: different case and slashes are the same folder
		must[Project](t)(f.store.AddProject(strings.ToUpper(f.project[:1]) + strings.ReplaceAll(f.project[1:], `\`, "/")))
	}
	if n := len(must[[]Project](t)(f.store.Projects())); n != 1 {
		t.Fatalf("projects after re-add = %d", n)
	}
	f.reopen()
	got := must[Session](t)(f.store.Session(s.ID))
	if got.ProjectID != s.ProjectID {
		t.Fatalf("project id %q != %q", got.ProjectID, s.ProjectID)
	}
}

func TestStoreEventsStateAndReplayOffsetAreDurable(t *testing.T) {
	f := newStoreFixture(t)
	sid := f.session().ID
	first := must[Record](t)(f.store.Append(sid, map[string]any{"type": "text.delta", "text": "a"}, map[string]any{"history": []any{"one"}}))
	second := must[Record](t)(f.store.Append(sid, map[string]any{"type": "text.delta", "text": "b"}, map[string]any{"history": []any{"two"}}))
	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("seqs %d %d", first.Seq, second.Seq)
	}
	after := must[[]Record](t)(f.store.Events(sid, 1, 1000))
	if len(after) != 1 || after[0].Payload != second.Payload {
		t.Fatalf("events after 1 = %+v", after)
	}
	state := must[map[string]any](t)(f.store.PlayerState(sid))
	if !reflect.DeepEqual(state, map[string]any{"history": []any{"two"}}) {
		t.Fatalf("state = %v", state)
	}
	data, err := os.ReadFile(f.store.TranscriptPath(sid))
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		texts = append(texts, m["text"].(string))
	}
	if !reflect.DeepEqual(texts, []string{"a", "b"}) {
		t.Fatalf("transcript texts = %v", texts)
	}
	// Python's json.dumps layout: type first, ", " / ": " separators, then seq and timestamp.
	if !strings.HasPrefix(first.Payload, `{"type": "text.delta", "text": "a", "seq": 1, "timestamp": `) {
		t.Fatalf("payload layout = %s", first.Payload)
	}
}

func TestStorePartialTranscriptMirrorIsRepairedFromJournal(t *testing.T) {
	f := newStoreFixture(t)
	sid := f.session().ID
	must[Record](t)(f.store.Append(sid, map[string]any{"type": "text.delta", "text": "durable"}, nil))
	f.store.Close()
	path := filepath.Join(f.data, "sessions", sid+".jsonl")
	if err := os.WriteFile(path, []byte("{partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.store = openTestStore(t, f.data)
	data, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil || m["text"] != "durable" {
		t.Fatalf("repaired mirror = %q (%v)", data, err)
	}
}

func TestStoreStartupKeepsCompleteTranscriptFileUntouched(t *testing.T) {
	f := newStoreFixture(t)
	sid := f.session().ID
	must[Record](t)(f.store.Append(sid, map[string]any{"type": "text.delta", "text": "keep ü"}, nil))
	path := f.store.TranscriptPath(sid)
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatalf("complete mirror was rewritten (mtime %v, want %v)", info.ModTime(), old)
	}
}

func TestStoreRestartMarksRunningSessionInterruptedOnce(t *testing.T) {
	f := newStoreFixture(t)
	sid := f.session().ID
	if err := f.store.UpdateSession(sid, map[string]any{"status": "running"}); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if s := must[Session](t)(f.store.Session(sid)); s.Status != "interrupted" {
		t.Fatalf("status = %s", s.Status)
	}
	events := must[[]Record](t)(f.store.Events(sid, 0, 1000))
	if events[len(events)-1].Fields()["stop_reason"] != "interrupted" {
		t.Fatalf("last event = %s", events[len(events)-1].Payload)
	}
	f.reopen()
	if n := len(must[[]Record](t)(f.store.Events(sid, 0, 1000))); n != 1 {
		t.Fatalf("events after second restart = %d", n)
	}
}

func TestStoreTerminalEventAndStatusAreAtomicAcrossRestart(t *testing.T) {
	f := newStoreFixture(t)
	sid := f.session().ID
	if err := f.store.UpdateSession(sid, map[string]any{"status": "running"}); err != nil {
		t.Fatal(err)
	}
	must[Record](t)(f.store.Append(sid, map[string]any{"type": "turn.completed", "stop_reason": "end_turn"}, nil))
	f.reopen()
	if s := must[Session](t)(f.store.Session(sid)); s.Status != "idle" {
		t.Fatalf("status = %s", s.Status)
	}
	events := must[[]Record](t)(f.store.Events(sid, 0, 1000))
	if len(events) != 1 || events[0].Fields()["stop_reason"] != "end_turn" {
		t.Fatalf("events = %v", events)
	}
}

func TestStoreSettingsRejectUnknownKeysAndNeverAcceptSecrets(t *testing.T) {
	f := newStoreFixture(t)
	if _, err := f.store.SetSettings(map[string]any{"default_player": "claude", "permission": "read-only"}); err != nil {
		t.Fatal(err)
	}
	if got := must[orderedObject](t)(f.store.Settings()).Get("default_player"); got != "claude" {
		t.Fatalf("default_player = %v", got)
	}
	for _, data := range []map[string]any{{"api_key": "never-store"}, {"permission": "bad"}, {"default_player": "fake"}} {
		var re *RequestError
		if _, err := f.store.SetSettings(data); !errors.As(err, &re) {
			t.Fatalf("SetSettings(%v) err = %v", data, err)
		}
	}
}

func TestStoreInvalidPathsIDsAndAgentsAreRejected(t *testing.T) {
	f := newStoreFixture(t)
	file := filepath.Join(f.root, "a-file")
	os.WriteFile(file, []byte("x"), 0o600)
	for _, path := range []any{filepath.Join(f.root, "missing"), file, "", "   ", nil, 5} {
		var re *RequestError
		if _, err := f.store.AddProject(path); !errors.As(err, &re) {
			t.Fatalf("AddProject(%v) err = %v", path, err)
		}
	}
	var nf *NotFoundError
	if _, err := f.store.Session("../escape"); !errors.As(err, &nf) {
		t.Fatalf("Session(../escape) err = %v", err)
	}
	p := must[Project](t)(f.store.AddProject(f.project))
	cases := []struct {
		player, model, permission, title any
		want                             string
	}{
		{"unknown", nil, "edit", nil, "Unknown player"},
		{"codex", nil, "root", nil, "Unknown permission mode"},
		{"codex", " ", "edit", nil, "model must be a non-empty string or null"},
		{"openrouter", nil, "edit", nil, "OpenRouter requires a model"},
		{"codex", nil, "edit", strings.Repeat("x", 201), "title must contain 1 to 200 characters"},
	}
	for _, c := range cases {
		_, err := f.store.CreateSession(p.ID, c.player, c.model, c.permission, c.title, nil)
		var re *RequestError
		if !errors.As(err, &re) || re.Msg != c.want {
			t.Fatalf("CreateSession(%v) err = %v, want %q", c, err, c.want)
		}
	}
}

func TestStoreSecondServerOnSameDataDirIsRefused(t *testing.T) {
	f := newStoreFixture(t)
	if _, err := OpenStore(f.data, StoreOptions{}); !errors.Is(err, ErrServerOwned) {
		t.Fatalf("second OpenStore err = %v", err)
	}
	f.store.Close()
	again := openTestStore(t, f.data) // closing released the OS lock
	again.Close()
	f.store = openTestStore(t, f.data)
}

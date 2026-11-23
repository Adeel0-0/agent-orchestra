package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Adeel0-0/agent-orchestra/internal/players"
)

const (
	testToken = "test-launch-token-that-is-not-a-real-secret"
	testPort  = 8765
)

var testBase = fmt.Sprintf("http://127.0.0.1:%d", testPort)

type apiFixture struct {
	t       *testing.T
	root    string
	project string
	app     *App
	player  *controlledPlayer
}

func newAPIFixture(t *testing.T, mutate func(*Config)) *apiFixture {
	root, project := tempLayout(t)
	f := &apiFixture{t: t, root: root, project: project, player: newControlledPlayer()}
	cfg := Config{
		Store: openTestStore(t, filepath.Join(root, "data")), Port: testPort, Token: testToken,
		Factory: func(Session, string) (players.Player, error) { return f.player, nil },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	f.app = must[*App](t)(New(cfg))
	t.Cleanup(func() { f.app.Close() })
	return f
}

type resp struct {
	code   int
	body   string
	header http.Header
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatalf("not a JSON object (%d): %s", r.code, r.body)
	}
	return m
}

// do sends a request through the full handler; headers default to the bearer token.
func (f *apiFixture) do(method, path string, body any, headers map[string]string) resp {
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		reader = strings.NewReader(string(data))
	}
	req := httptest.NewRequest(method, testBase+path, reader)
	req.Header.Set("Authorization", "Bearer "+testToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		switch {
		case k == "Host":
			req.Host = v
		case v == "":
			req.Header.Del(k)
		default:
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	f.app.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Body.String(), rec.Header()}
}

func TestAPIProjectSessionSettingsAndDiffEndpoints(t *testing.T) {
	f := newAPIFixture(t, nil)
	r := f.do("POST", "/api/projects", map[string]any{"path": f.project}, nil)
	if r.code != 201 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	pid := r.json(t)["id"].(string)
	var list []any
	json.Unmarshal([]byte(f.do("GET", "/api/projects", nil, nil).body), &list)
	if len(list) != 1 {
		t.Fatalf("projects = %v", list)
	}
	r = f.do("POST", "/api/projects/"+pid+"/sessions", map[string]any{"player": "codex", "title": "test"}, nil)
	if r.code != 201 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	session := r.json(t)
	sid := session["id"].(string)
	for _, key := range []string{"id", "project_id", "player", "model", "title", "worktree", "backend_session_id",
		"status", "created", "updated", "permission", "base_commit"} {
		if _, ok := session[key]; !ok {
			t.Fatalf("session JSON lacks %q: %s", key, r.body)
		}
	}
	json.Unmarshal([]byte(f.do("GET", "/api/projects/"+pid+"/sessions", nil, nil).body), &list)
	if len(list) != 1 {
		t.Fatalf("sessions = %v", list)
	}
	r = f.do("GET", "/api/sessions/"+sid+"/diff", nil, nil)
	if r.code != 200 || r.body != `{"base_commit":null,"diff":"","untracked":[],"truncated":false}` {
		t.Fatalf("diff %d %s", r.code, r.body)
	}
	r = f.do("PUT", "/api/settings", map[string]any{"permission": "read-only"}, nil)
	if r.code != 200 || r.body != `{"default_player":"codex","permission":"read-only","openrouter_key_present":false}` {
		t.Fatalf("settings %d %s", r.code, r.body)
	}
	r = f.do("GET", "/api/capabilities", nil, nil)
	if r.body != `{"native_folder_picker":false}` {
		t.Fatalf("capabilities %s", r.body)
	}
	r = f.do("POST", "/api/folders/pick", map[string]any{}, nil)
	if r.code != 400 || !strings.Contains(r.body, "Native folder picker is unavailable") {
		t.Fatalf("pick %d %s", r.code, r.body)
	}
}

func TestAPIMessagesCancelAndPermissionRoutes(t *testing.T) {
	f := newAPIFixture(t, nil)
	f.player.permission = true
	pid := f.do("POST", "/api/projects", map[string]any{"path": f.project}, nil).json(t)["id"].(string)
	sid := f.do("POST", "/api/projects/"+pid+"/sessions", map[string]any{}, nil).json(t)["id"].(string)
	r := f.do("POST", "/api/sessions/"+sid+"/messages", map[string]any{"prompt": "\n  Explain the parser\nin detail"}, nil)
	if r.code != 202 || r.body != `{"session_id":"`+sid+`","status":"running"}` {
		t.Fatalf("messages %d %s", r.code, r.body)
	}
	if title := must[Session](t)(f.app.Store.Session(sid)).Title; title != "Explain the parser" {
		t.Fatalf("title = %q", title)
	}
	if r := f.do("POST", "/api/sessions/"+sid+"/messages", map[string]any{"prompt": "again"}, nil); r.code != 409 {
		t.Fatalf("second turn %d %s", r.code, r.body)
	}
	eventually(t, "permission request", func() bool {
		return must[Session](t)(f.app.Store.Session(sid)).Status == "waiting_permission"
	})
	if r := f.do("POST", "/api/sessions/"+sid+"/permissions/approval", map[string]any{"allow": "yes"}, nil); r.code != 400 {
		t.Fatalf("non-bool allow %d %s", r.code, r.body)
	}
	if r := f.do("POST", "/api/sessions/"+sid+"/permissions/other", map[string]any{"allow": true}, nil); r.code != 409 {
		t.Fatalf("unknown request %d %s", r.code, r.body)
	}
	f.player.permission = false
	if r := f.do("POST", "/api/sessions/"+sid+"/permissions/approval", map[string]any{"allow": true}, nil); r.code != 200 || r.body != `{"ok":true}` {
		t.Fatalf("answer %d %s", r.code, r.body)
	}
	f.app.Manager.Wait(sid)
	f.player.proceed.Clear()
	f.do("POST", "/api/sessions/"+sid+"/messages", map[string]any{"prompt": "long"}, nil)
	if r := f.do("POST", "/api/sessions/"+sid+"/cancel", map[string]any{"confirm_stopped": "no"}, nil); r.code != 400 {
		t.Fatalf("non-bool confirm %d %s", r.code, r.body)
	}
	if r := f.do("POST", "/api/sessions/"+sid+"/cancel", nil, nil); r.code != 200 || r.body != `{"status":"cancelled"}` {
		t.Fatalf("cancel %d %s", r.code, r.body)
	}
	if r := f.do("POST", "/api/sessions/missing/cancel", nil, nil); r.code != 404 {
		t.Fatalf("unknown cancel %d %s", r.code, r.body)
	}
	if r := f.do("POST", "/api/sessions/missing/messages", map[string]any{"prompt": "x"}, nil); r.code != 404 {
		t.Fatalf("unknown messages %d %s", r.code, r.body)
	}
}

func TestAPIHostOriginTokenAndForwardingRestrictions(t *testing.T) {
	f := newAPIFixture(t, nil)
	for _, h := range []map[string]string{
		{"Authorization": ""}, {"Authorization": "Bearer wrong"}, {"Authorization": "Bearer "},
		{"Authorization": "bearer " + testToken}, {"Authorization": "Bearer " + testToken + "x"},
		{"Host": "evil.test"}, {"Host": fmt.Sprintf("localhost:%d", testPort)}, {"Host": "127.0.0.1:1"},
		{"Origin": "https://evil.test"}, {"Origin": "null"}, {"Origin": testBase + "/"},
		{"X-Forwarded-Host": fmt.Sprintf("127.0.0.1:%d", testPort)}, {"Forwarded": "host=evil.test"},
		{"X-Forwarded-For": "127.0.0.1"},
	} {
		r := f.do("GET", "/api/projects", nil, h)
		if r.code != 401 && r.code != 403 {
			t.Fatalf("%v -> %d %s", h, r.code, r.body)
		}
		if r.body != `{"error":"Local authorization required"}` || r.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%v rejection body %s", h, r.body)
		}
	}
	if r := f.do("GET", "/api/projects", nil, map[string]string{"Host": "evil.test"}); r.code != 403 {
		t.Fatalf("bad host should be 403, got %d", r.code)
	}
	if r := f.do("GET", "/api/projects", nil, map[string]string{"Authorization": ""}); r.code != 401 {
		t.Fatalf("missing token should be 401, got %d", r.code)
	}
	// Duplicate Authorization or Origin headers are refused.
	req := httptest.NewRequest("GET", testBase+"/api/projects", nil)
	req.Header.Add("Authorization", "Bearer "+testToken)
	req.Header.Add("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	f.app.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("duplicate authorization -> %d", rec.Code)
	}
	req = httptest.NewRequest("GET", testBase+"/api/projects", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Add("Origin", testBase)
	req.Header.Add("Origin", testBase)
	rec = httptest.NewRecorder()
	f.app.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("duplicate origin -> %d", rec.Code)
	}
	r := f.do("GET", "/api/projects", nil, map[string]string{"Origin": testBase})
	if r.code != 200 {
		t.Fatalf("exact origin -> %d %s", r.code, r.body)
	}
	for _, h := range []string{"X-Content-Type-Options", "Referrer-Policy", "Content-Security-Policy"} {
		if r.header.Get(h) == "" {
			t.Fatalf("missing %s", h)
		}
	}
	if !strings.Contains(r.header.Get("Content-Security-Policy"), "connect-src 'self' ws://127.0.0.1:8765;") {
		t.Fatalf("csp = %s", r.header.Get("Content-Security-Policy"))
	}
}

func TestAPIValidationErrorsAreCleanAndSecretsAreNotSettings(t *testing.T) {
	f := newAPIFixture(t, nil)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/projects", map[string]any{"path": "../missing"}},
		{"POST", "/api/projects", map[string]any{"path": f.project, "extra": 1}},
		{"POST", "/api/projects", []any{1}},
		{"PUT", "/api/settings", map[string]any{"api_key": "do-not-store"}},
		{"PUT", "/api/settings", map[string]any{"permission": []any{}}},
		{"PUT", "/api/settings", map[string]any{"default_player": map[string]any{}}},
		{"POST", "/api/projects", "not-json"},
		{"POST", "/api/projects", "{\"path\": \"\xff\"}"},
	} {
		r := f.do(c.method, c.path, c.body, nil)
		if r.code != 400 || strings.Contains(r.body, "do-not-store") {
			t.Fatalf("%v -> %d %s", c, r.code, r.body)
		}
		if _, ok := r.json(t)["error"].(string); !ok {
			t.Fatalf("no error message: %s", r.body)
		}
	}
	if r := f.do("POST", "/api/projects", map[string]any{"path": f.project}, map[string]string{"Content-Type": "text/plain"}); r.code != 400 ||
		r.json(t)["error"] != "Expected application/json" {
		t.Fatalf("wrong content type -> %d %s", r.code, r.body)
	}
	if r := f.do("GET", "/api/projects/missing/sessions", nil, nil); r.code != 404 || r.body != `{"error":"Unknown project"}` {
		t.Fatalf("missing project -> %d %s", r.code, r.body)
	}
	r := f.do("POST", "/api/projects", map[string]any{"path": strings.Repeat("x", 1100000)}, nil)
	if r.code != 413 || r.json(t)["error"] != "Request body exceeds 1048576 bytes" {
		t.Fatalf("oversized -> %d %s", r.code, r.body)
	}
	if r := f.do("DELETE", "/api/projects", nil, nil); r.code != 405 {
		t.Fatalf("DELETE -> %d", r.code)
	}
	if r := f.do("GET", "/api/nothing", nil, nil); r.code != 404 {
		t.Fatalf("unknown route -> %d", r.code)
	}
	// The UI routes do not exist in API-only mode.
	if r := f.do("POST", "/api/bootstrap", map[string]any{"nonce": "x"}, nil); r.code != 404 {
		t.Fatalf("bootstrap in API mode -> %d", r.code)
	}
	if r := f.do("GET", "/", nil, map[string]string{"Authorization": ""}); r.code != 401 {
		t.Fatalf("shell in API mode -> %d", r.code)
	}
}

func TestAPIUnexpectedHandlerErrorIsServerError(t *testing.T) {
	f := newAPIFixture(t, func(c *Config) {
		c.Factory = func(Session, string) (players.Player, error) { return nil, errors.New("implementation defect") }
	})
	pid := f.do("POST", "/api/projects", map[string]any{"path": f.project}, nil).json(t)["id"].(string)
	sid := f.do("POST", "/api/projects/"+pid+"/sessions", map[string]any{}, nil).json(t)["id"].(string)
	r := f.do("POST", "/api/sessions/"+sid+"/messages", map[string]any{"prompt": "hi"}, nil)
	if r.code != 500 || strings.Contains(r.body, "implementation defect") {
		t.Fatalf("-> %d %s", r.code, r.body)
	}
	// The failed start released the writer lock, so the error is not sticky.
	r = f.do("POST", "/api/sessions/"+sid+"/messages", map[string]any{"prompt": "hi"}, nil)
	if r.code != 500 {
		t.Fatalf("retry -> %d %s", r.code, r.body)
	}
}

func TestAPIPlayersCombinesHealthAndCatalog(t *testing.T) {
	path, version := "C:/bin/codex", "codex 1.0"
	no := false
	f := newAPIFixture(t, func(c *Config) {
		c.PlayerHealth = func(context.Context) []PlayerHealth {
			return []PlayerHealth{{ID: "codex", Name: "Codex", Path: &path, Version: &version},
				{ID: "openrouter", Name: "OpenRouter", LoggedIn: &no}}
		}
		c.Models = func(context.Context) ([]any, error) { return nil, errors.New("catalog offline") }
		c.KeyPresent = func() bool { return true }
	})
	r := f.do("GET", "/api/players", nil, nil)
	want := `[{"id":"codex","name":"Codex","path":"C:/bin/codex","version":"codex 1.0","logged_in":null,"fix":null,"ready":true,"models":[],"models_error":null},` +
		`{"id":"openrouter","name":"OpenRouter","path":null,"version":null,"logged_in":false,"fix":null,"ready":false,"models":[],"models_error":"catalog offline"}]`
	if r.code != 200 || r.body != want {
		t.Fatalf("players %d\n%s\nwant\n%s", r.code, r.body, want)
	}
	if r := f.do("GET", "/api/settings", nil, nil); !strings.HasSuffix(r.body, `"openrouter_key_present":true}`) {
		t.Fatalf("settings %s", r.body)
	}
}

func gitAvailable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGitDiffDrainsLargeOutputAndReportsExplicitCap(t *testing.T) {
	gitAvailable(t)
	_, project := tempLayout(t)
	runGit(t, project, "init", "-q")
	source := filepath.Join(project, "source.txt")
	os.WriteFile(source, []byte("old\n"), 0o600)
	runGit(t, project, "add", "source.txt")
	runGit(t, project, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-q", "-m", "fixture")
	commit := runGit(t, project, "rev-parse", "HEAD")
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, "line-%06d\n", i)
	}
	os.WriteFile(source, []byte(b.String()), 0o600)
	out, truncated, err := gitRead(project, gitOutputLimit, "diff", "--no-ext-diff", "--no-textconv", commit, "--")
	if err != nil || out == nil || truncated || !strings.Contains(*out, "+line-019999") {
		t.Fatalf("full diff: truncated=%v err=%v", truncated, err)
	}
	out, truncated, err = gitRead(project, 2000, "diff", "--no-ext-diff", "--no-textconv", commit, "--")
	if err != nil || out == nil || !truncated || len(*out) > 2000 {
		t.Fatalf("capped diff: truncated=%v err=%v", truncated, err)
	}
	hook := filepath.Join(project, ".git", "fsmonitor-hook")
	os.WriteFile(hook, []byte("#!/bin/sh\nprintf bad > fsmonitor-executed.txt\nprintf \"\\000\"\n"), 0o700)
	runGit(t, project, "config", "core.fsmonitor", filepath.ToSlash(hook))
	if _, _, err := gitRead(project, gitOutputLimit, "diff", "--no-ext-diff", "--no-textconv", commit, "--"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, "fsmonitor-executed.txt")); err == nil {
		t.Fatal("diff executed a repository hook")
	}
	if out, _, _ := gitRead(project, gitOutputLimit, "diff", "no-such-base", "--"); out != nil {
		t.Fatal("failing git command returned output")
	}
}

func TestAPIDiffAgainstSavedBaseListsUntracked(t *testing.T) {
	gitAvailable(t)
	f := newAPIFixture(t, nil)
	runGit(t, f.project, "init", "-q")
	os.WriteFile(filepath.Join(f.project, "a.txt"), []byte("one\n"), 0o600)
	runGit(t, f.project, "add", "a.txt")
	runGit(t, f.project, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-q", "-m", "base")
	head := runGit(t, f.project, "rev-parse", "HEAD")
	pid := f.do("POST", "/api/projects", map[string]any{"path": f.project}, nil).json(t)["id"].(string)
	s := f.do("POST", "/api/projects/"+pid+"/sessions", map[string]any{}, nil).json(t)
	if s["base_commit"] != head {
		t.Fatalf("base_commit = %v, want %s", s["base_commit"], head)
	}
	os.WriteFile(filepath.Join(f.project, "a.txt"), []byte("two\n"), 0o600)
	os.WriteFile(filepath.Join(f.project, "new file.txt"), []byte("x"), 0o600)
	d := f.do("GET", "/api/sessions/"+s["id"].(string)+"/diff", nil, nil).json(t)
	if !strings.Contains(d["diff"].(string), "+two") || d["truncated"] != false {
		t.Fatalf("diff = %v", d)
	}
	if u := d["untracked"].([]any); len(u) != 1 || u[0] != "new file.txt" {
		t.Fatalf("untracked = %v", u)
	}
}

// ---- WebSocket boundary

func TestWebSocketUnauthenticatedOrCrossOriginIsRejectedBeforeAccept(t *testing.T) {
	reached := false
	sec := newLocalSecurity(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }), testPort, testToken, nil)
	for _, extra := range []map[string]string{
		{},
		{"Origin": "https://evil.test", "Sec-WebSocket-Protocol": "orchestra, orchestra.token." + testToken},
		{"Sec-WebSocket-Protocol": "orchestra.token." + testToken},                                        // no "orchestra"
		{"Sec-WebSocket-Protocol": "orchestra, orchestra.token." + testToken + ", orchestra.token.other"}, // two tokens
	} {
		req := httptest.NewRequest("GET", testBase+"/api/sessions/x/events", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		sec.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Fatalf("%v -> %d", extra, rec.Code)
		}
	}
	if reached {
		t.Fatal("rejected socket reached the handler")
	}
	// The subprotocol credential never authenticates plain HTTP.
	req := httptest.NewRequest("GET", testBase+"/api/projects", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "orchestra, orchestra.token."+testToken)
	rec := httptest.NewRecorder()
	sec.ServeHTTP(rec, req)
	if rec.Code != 401 || reached {
		t.Fatalf("subprotocol on HTTP -> %d", rec.Code)
	}
}

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"github.com/Adeel0-0/agent-orchestra/internal/players"
)

const liveToken = "local-integration-test-token-not-a-credential"

// socketPlayer is the live test's FakePlayer: it resumes the persisted backend id.
type socketPlayer struct {
	sessionID string
	gate      *gate
	record    func(prompt, sessionID string)
}

func (p *socketPlayer) ID() string                          { return "codex" }
func (p *socketPlayer) SessionID() string                   { return p.sessionID }
func (p *socketPlayer) AnswerPermission(string, bool) error { return errors.New("unsupported") }

func (p *socketPlayer) Send(ctx context.Context, prompt string, emit func(players.Event)) error {
	p.record(prompt, p.sessionID)
	emit(players.NewEvent(players.SessionStarted, map[string]any{"backend_session_id": p.sessionID, "model": nil}))
	emit(players.NewEvent(players.TextDelta, map[string]any{"text": "first"}))
	select {
	case <-p.gate.C():
	case <-ctx.Done():
		return ctx.Err()
	}
	emit(players.NewEvent(players.TextDelta, map[string]any{"text": "last"}))
	emit(players.NewEvent(players.TurnCompleted, map[string]any{"stop_reason": "end_turn"}))
	return nil
}

type client struct {
	t      *testing.T
	base   string
	token  string
	client *http.Client
}

func (c client) call(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, c.base+path, reader)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.client.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, data
}

func (c client) object(method, path string, body any) map[string]any {
	c.t.Helper()
	code, data := c.call(method, path, body)
	var m map[string]any
	if code >= 300 || json.Unmarshal(data, &m) != nil {
		c.t.Fatalf("%s %s -> %d %s", method, path, code, data)
	}
	return m
}

func dialEvents(t *testing.T, rt *Runtime, sid string, after int64) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := fmt.Sprintf("ws://127.0.0.1:%d/api/sessions/%s/events?after=%d", rt.Port, sid, after)
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		Subprotocols: []string{"orchestra", "orchestra.token." + rt.App.Token},
		HTTPHeader:   http.Header{"Origin": {rt.Origin}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if conn.Subprotocol() != "orchestra" {
		t.Fatalf("selected subprotocol %q", conn.Subprotocol())
	}
	return conn
}

func readEvent(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLiveDisconnectReplayRestartAndFollowupOverRealSockets(t *testing.T) {
	root, project := tempLayout(t)
	data := filepath.Join(root, "data")
	g := newGate()
	var mu sync.Mutex
	var received [][2]string
	factory := func(s Session, cwd string) (players.Player, error) {
		id := "real-socket-backend"
		if s.BackendSessionID != nil {
			id = *s.BackendSessionID
		}
		return &socketPlayer{sessionID: id, gate: g, record: func(p, id string) {
			mu.Lock()
			received = append(received, [2]string{p, id})
			mu.Unlock()
		}}, nil
	}
	start := func() *Runtime {
		rt, err := StartRuntime(LaunchConfig{DataDir: data, App: Config{Token: liveToken, Factory: factory}})
		if err != nil {
			t.Fatal(err)
		}
		return rt
	}

	rt := start()
	c := client{t: t, base: rt.Origin, token: liveToken, client: &http.Client{Timeout: 10 * time.Second}}
	pid := c.object("POST", "/api/projects", map[string]any{"path": project})["id"].(string)
	sid := c.object("POST", "/api/projects/"+pid+"/sessions", map[string]any{"player": "codex"})["id"].(string)
	ws := dialEvents(t, rt, sid, 0)
	if code, body := c.call("POST", "/api/sessions/"+sid+"/messages", map[string]any{"prompt": "first turn"}); code != 202 {
		t.Fatalf("messages %d %s", code, body)
	}
	first := readEvent(t, ws)
	if first["type"] != "message.user" {
		t.Fatalf("first = %v", first)
	}
	ws.CloseNow() // the UI disconnects while the player is blocked
	g.Set()
	rt.App.Manager.Wait(sid)
	ws = dialEvents(t, rt, sid, int64(first["seq"].(float64)))
	var texts []string
	for {
		e := readEvent(t, ws)
		if e["type"] == players.TextDelta {
			texts = append(texts, e["text"].(string))
		}
		if e["type"] == players.TurnCompleted {
			break
		}
	}
	ws.CloseNow()
	if strings.Join(texts, ",") != "first,last" {
		t.Fatalf("texts = %v", texts)
	}
	if err := rt.Stop(); err != nil {
		t.Fatal(err)
	}

	rt = start()
	defer rt.Stop()
	c.base = rt.Origin
	code, body := c.call("GET", "/api/projects/"+pid+"/sessions", nil)
	var sessions []map[string]any
	if code != 200 || json.Unmarshal(body, &sessions) != nil || sessions[0]["backend_session_id"] != "real-socket-backend" {
		t.Fatalf("sessions after restart %d %s", code, body)
	}
	if code, body := c.call("POST", "/api/sessions/"+sid+"/messages", map[string]any{"prompt": "follow up"}); code != 202 {
		t.Fatalf("follow up %d %s", code, body)
	}
	rt.App.Manager.Wait(sid)
	mu.Lock()
	last := received[len(received)-1]
	mu.Unlock()
	if last != [2]string{"follow up", "real-socket-backend"} {
		t.Fatalf("last received = %v", last)
	}
}

func TestLiveWebSocketBoundary(t *testing.T) {
	_, project := tempLayout(t)
	rt, err := StartRuntime(LaunchConfig{DataDir: t.TempDir(), App: Config{Token: liveToken,
		Factory: func(Session, string) (players.Player, error) { return newControlledPlayer(), nil }}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()
	c := client{t: t, base: rt.Origin, token: liveToken, client: &http.Client{Timeout: 10 * time.Second}}
	pid := c.object("POST", "/api/projects", map[string]any{"path": project})["id"].(string)
	sid := c.object("POST", "/api/projects/"+pid+"/sessions", map[string]any{})["id"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	base := fmt.Sprintf("ws://127.0.0.1:%d/api/sessions/%s/events", rt.Port, sid)
	cases := map[string]struct {
		url  string
		opts *websocket.DialOptions
	}{
		"no token":         {base, &websocket.DialOptions{Subprotocols: []string{"orchestra"}}},
		"wrong token":      {base, &websocket.DialOptions{Subprotocols: []string{"orchestra", "orchestra.token.wrong-token-wrong-token-xx"}}},
		"foreign origin":   {base, &websocket.DialOptions{Subprotocols: []string{"orchestra", "orchestra.token." + liveToken}, HTTPHeader: http.Header{"Origin": {"https://evil.test"}}}},
		"query token":      {base + "?token=" + liveToken, nil},
		"offset too large": {base + "?after=5", &websocket.DialOptions{Subprotocols: []string{"orchestra", "orchestra.token." + liveToken}}},
		"bad offset":       {base + "?after=x", &websocket.DialOptions{Subprotocols: []string{"orchestra", "orchestra.token." + liveToken}}},
		"unknown session":  {fmt.Sprintf("ws://127.0.0.1:%d/api/sessions/missing/events", rt.Port), &websocket.DialOptions{Subprotocols: []string{"orchestra", "orchestra.token." + liveToken}}},
		"localhost":        {fmt.Sprintf("ws://localhost:%d/api/sessions/%s/events", rt.Port, sid), &websocket.DialOptions{Subprotocols: []string{"orchestra", "orchestra.token." + liveToken}}},
	}
	for name, tc := range cases {
		conn, res, err := websocket.Dial(ctx, tc.url, tc.opts)
		if err == nil {
			conn.CloseNow()
			t.Fatalf("%s: socket accepted", name)
		}
		if res == nil || res.StatusCode != 403 {
			if name == "localhost" && res == nil {
				continue // localhost may resolve to ::1 only, which has no listener
			}
			t.Fatalf("%s: response %v (%v)", name, res, err)
		}
	}
	// A native client may use the Bearer header instead of the subprotocol.
	conn, _, err := websocket.Dial(ctx, base, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + liveToken}}})
	if err != nil {
		t.Fatal(err)
	}
	if conn.Subprotocol() != "" {
		t.Fatalf("subprotocol %q", conn.Subprotocol())
	}
	conn.CloseNow()
	// Plain HTTP on the events route finds no route.
	if code, _ := c.call("GET", "/api/sessions/"+sid+"/events", nil); code != 404 {
		t.Fatalf("plain GET events -> %d", code)
	}
}

func TestLiveShutdownClosesSubscribersWithTryAgainLater(t *testing.T) {
	_, project := tempLayout(t)
	rt, err := StartRuntime(LaunchConfig{DataDir: t.TempDir(), App: Config{Token: liveToken,
		Factory: func(Session, string) (players.Player, error) { return newControlledPlayer(), nil }}})
	if err != nil {
		t.Fatal(err)
	}
	c := client{t: t, base: rt.Origin, token: liveToken, client: &http.Client{Timeout: 10 * time.Second}}
	pid := c.object("POST", "/api/projects", map[string]any{"path": project})["id"].(string)
	sid := c.object("POST", "/api/projects/"+pid+"/sessions", map[string]any{})["id"].(string)
	c.call("POST", "/api/sessions/"+sid+"/messages", map[string]any{"prompt": "long turn"})
	ws := dialEvents(t, rt, sid, 0)
	defer ws.CloseNow()
	readEvent(t, ws)
	go rt.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, _, err := ws.Read(ctx)
		if err == nil {
			continue
		}
		if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
			t.Fatalf("close = %v", err)
		}
		break
	}
	<-rt.Done()
	eventually(t, "store closed", func() bool {
		rt.App.Store.mu.Lock()
		defer rt.App.Store.mu.Unlock()
		return rt.App.Store.closed
	})
}

// ---- launcher

func TestLauncherSocketIsRandomPortAndIPv4Loopback(t *testing.T) {
	ln, err := ReserveListener()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)
	if addr.IP.String() != "127.0.0.1" || addr.Port <= 0 {
		t.Fatalf("addr = %v", addr)
	}
}

func TestLauncherRuntimeOwnsServerAndNonceNeverContainsAPIToken(t *testing.T) {
	rt, err := StartRuntime(LaunchConfig{DataDir: t.TempDir(), App: Config{Web: testWeb(),
		Factory: func(Session, string) (players.Player, error) { return newControlledPlayer(), nil }}})
	if err != nil {
		t.Fatal(err)
	}
	url := rt.BootstrapURL()
	if strings.Contains(url, rt.App.Token) || !strings.HasPrefix(url, rt.Origin+"/#bootstrap=") {
		t.Fatalf("bootstrap url %s", url)
	}
	nonce := strings.SplitN(url, "#bootstrap=", 2)[1]
	req, _ := http.NewRequest("POST", rt.Origin+"/api/bootstrap", strings.NewReader(`{"nonce":"`+nonce+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", rt.Origin)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()
	if body["token"] != rt.App.Token {
		t.Fatalf("exchange = %v", body)
	}
	if err := rt.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rt.Done():
	default:
		t.Fatal("server still running after Stop")
	}
	if _, err := http.Get(rt.Origin + "/"); err == nil {
		t.Fatal("server still accepting after Stop")
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestLauncherServePrintsURLAndTokenAndServesAPIOnly(t *testing.T) {
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, LaunchConfig{DataDir: t.TempDir(), Out: out, App: Config{Web: fstest.MapFS{},
			Factory: func(Session, string) (players.Player, error) { return newControlledPlayer(), nil }}})
	}()
	eventually(t, "serve output", func() bool { return strings.Contains(out.String(), "This command serves the API only.") })
	lines := strings.Split(out.String(), "\n")
	origin := strings.TrimPrefix(lines[0], "Orchestra API: ")
	token := strings.TrimPrefix(lines[1], "Authorization: Bearer ")
	if !strings.HasPrefix(origin, "http://127.0.0.1:") || len(token) < 24 {
		t.Fatalf("output %q", out.String())
	}
	c := client{t: t, base: origin, token: token, client: &http.Client{Timeout: 5 * time.Second}}
	if code, _ := c.call("GET", "/api/projects", nil); code != 200 {
		t.Fatalf("projects -> %d", code)
	}
	c.token = ""
	if code, _ := c.call("GET", "/", nil); code != 401 {
		t.Fatalf("UI shell in serve mode -> %d", code)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestLauncherRunUIOpensBrowserWithBootstrapURL(t *testing.T) {
	out := &syncBuffer{}
	opened := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunUI(ctx, LaunchConfig{DataDir: t.TempDir(), Out: out,
			OpenBrowser: func(url string) error { opened <- url; return nil },
			App: Config{Web: testWeb(), FolderPicker: func(context.Context) (string, error) { return "x", nil },
				Factory: func(Session, string) (players.Player, error) { return newControlledPlayer(), nil }}})
	}()
	var url string
	select {
	case url = <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("browser not opened")
	}
	origin := strings.SplitN(url, "/#bootstrap=", 2)[0]
	res, err := http.Get(origin + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("shell -> %d", res.StatusCode)
	}
	nonce := strings.SplitN(url, "#bootstrap=", 2)[1]
	req, _ := http.NewRequest("POST", origin+"/api/bootstrap", strings.NewReader(`{"nonce":"`+nonce+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()
	c := client{t: t, base: origin, token: body["token"], client: &http.Client{Timeout: 5 * time.Second}}
	if m := c.object("GET", "/api/capabilities", nil); m["native_folder_picker"] != false {
		t.Fatalf("browser mode offers a folder picker: %v", m)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Orchestra is open in your browser.") {
		t.Fatalf("output %q", out.String())
	}
	if err := RunUI(context.Background(), LaunchConfig{App: Config{}}); err != ErrUIAssetsMissing {
		t.Fatalf("RunUI without assets = %v", err)
	}
}

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	maxBodyBytes      = 1024 * 1024
	maxBootstrapBytes = 1024
	gitOutputLimit    = 1024 * 1024
	gitTimeout        = 10 * time.Second
	wsShutdownWait    = 10 * time.Second
)

// PlayerHealth is one installed-player check, as Python's registry.PlayerHealth.
type PlayerHealth struct {
	ID       string
	Name     string
	Path     *string // nil: not installed
	Version  *string
	LoggedIn *bool // nil: the CLI has no way to check
	Fix      *string
}

// Ready mirrors PlayerHealth.ready: installed and not known to be logged out.
func (h PlayerHealth) Ready() bool { return h.Path != nil && (h.LoggedIn == nil || *h.LoggedIn) }

// Config is everything the server needs; the CLI supplies real implementations.
type Config struct {
	Store *Store // required; the App closes it
	Port  int    // the bound 127.0.0.1 port (1-65535)
	Token string // launch token; "" generates one

	Factory Factory // required: builds a player for a session turn

	// PlayerHealth checks local players for GET /api/players (nil: none).
	PlayerHealth func(ctx context.Context) []PlayerHealth
	// Models returns the tools-capable OpenRouter catalog (nil: empty, no error).
	// Its error text is shown to the user as models_error.
	Models func(ctx context.Context) ([]any, error)
	// KeyPresent reports whether an OpenRouter key is in the OS keyring (nil: false).
	// It must never return or log the key.
	KeyPresent func() bool
	// FolderPicker opens a native folder dialog; "" means the user cancelled.
	// nil in browser mode.
	FolderPicker func(ctx context.Context) (string, error)
	// Web is the built UI (index.html and assets/). nil serves the API only. Use an
	// embed.FS or a trusted directory: symlinks inside an os.DirFS are followed.
	Web fs.FS
}

// App is the local API: an http.Handler plus the session manager it owns.
type App struct {
	Store     *Store
	Manager   *Manager
	Token     string
	Port      int
	Bootstrap *BootstrapTokens

	cfg     Config
	origin  string
	handler http.Handler

	wsMu     sync.Mutex
	wsClosed bool
	ws       sync.WaitGroup
	once     sync.Once
	closeErr error
}

// New builds the app (orchestra/app/server.py create_app).
func New(cfg Config) (*App, error) {
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, requestErr("Expected a bound loopback port")
	}
	if cfg.Token == "" {
		cfg.Token = tokenURLSafe(32)
	}
	if !isASCII(cfg.Token) || len(cfg.Token) < 24 {
		return nil, requestErr("Launch token must contain at least 24 ASCII characters")
	}
	if cfg.Store == nil {
		return nil, errors.New("app: Config.Store is required")
	}
	if cfg.Factory == nil {
		return nil, errors.New("app: Config.Factory is required")
	}
	if cfg.Web != nil {
		if err := checkWebAssets(cfg.Web); err != nil {
			return nil, err
		}
	}
	a := &App{
		Store: cfg.Store, Manager: NewManager(cfg.Store, cfg.Factory), Token: cfg.Token, Port: cfg.Port,
		Bootstrap: NewBootstrapTokens(), cfg: cfg, origin: fmt.Sprintf("http://127.0.0.1:%d", cfg.Port),
	}
	var public func(*http.Request) bool
	if cfg.Web != nil {
		public = publicUIRequest
	}
	a.handler = newLocalSecurity(http.HandlerFunc(a.route), cfg.Port, cfg.Token, public)
	return a, nil
}

// ServeHTTP serves the API behind the loopback/token checks.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

// Close cancels running turns (waiting for their cleanup), disconnects event
// subscribers and closes the store. Call it after the HTTP server stopped accepting.
func (a *App) Close() error {
	a.once.Do(func() {
		a.Manager.Close()
		a.wsMu.Lock()
		a.wsClosed = true
		a.wsMu.Unlock()
		done := make(chan struct{})
		go func() { a.ws.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(wsShutdownWait):
			log.Printf("orchestra app: event streams still closing at shutdown")
		}
		a.closeErr = a.Store.Close()
	})
	return a.closeErr
}

type apiHandler func(w http.ResponseWriter, r *http.Request) error

func (a *App) serve(w http.ResponseWriter, r *http.Request, h apiHandler) {
	err := h(w, r)
	if err == nil {
		return
	}
	status, explicit := statusFor(err)
	if !explicit {
		log.Printf("orchestra app: %s %s failed: %v", r.Method, r.URL.Path, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(bytes.TrimRight(buf.Bytes(), "\n"))
}

// methodAllowed answers 405 unless r.Method is listed (GET implies HEAD).
func methodAllowed(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m || (m == http.MethodGet && r.Method == http.MethodHead) {
			return true
		}
	}
	allow := strings.Join(methods, ", ")
	for _, m := range methods {
		if m == http.MethodGet {
			allow += ", HEAD"
		}
	}
	w.Header().Set("Allow", allow)
	http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	return false
}

func (a *App) route(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if isWebSocket(r) {
		if len(parts) == 4 && parts[0] == "api" && parts[1] == "sessions" && parts[2] != "" && parts[3] == "events" {
			a.events(w, r, parts[2])
			return
		}
		reject(w)
		return
	}
	switch {
	case p == "/api/capabilities":
		if methodAllowed(w, r, http.MethodGet) {
			writeJSON(w, 200, map[string]bool{"native_folder_picker": a.cfg.FolderPicker != nil})
		}
	case p == "/api/folders/pick":
		if methodAllowed(w, r, http.MethodPost) {
			a.serve(w, r, a.pickFolder)
		}
	case p == "/api/players":
		if methodAllowed(w, r, http.MethodGet) {
			a.serve(w, r, a.players)
		}
	case p == "/api/projects":
		if methodAllowed(w, r, http.MethodGet, http.MethodPost) {
			a.serve(w, r, a.projects)
		}
	case p == "/api/settings":
		if methodAllowed(w, r, http.MethodGet, http.MethodPut) {
			a.serve(w, r, a.settings)
		}
	case len(parts) == 4 && parts[0] == "api" && parts[1] == "projects" && parts[2] != "" && parts[3] == "sessions":
		if methodAllowed(w, r, http.MethodGet, http.MethodPost) {
			a.serve(w, r, func(w http.ResponseWriter, r *http.Request) error { return a.sessions(w, r, parts[2]) })
		}
	case len(parts) >= 4 && parts[0] == "api" && parts[1] == "sessions" && parts[2] != "":
		a.sessionRoute(w, r, parts[2], parts[3:])
	case a.cfg.Web != nil && p == "/":
		if methodAllowed(w, r, http.MethodGet) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			serveFile(w, r, a.cfg.Web, "index.html")
		}
	case a.cfg.Web != nil && p == "/api/bootstrap":
		if methodAllowed(w, r, http.MethodPost) {
			a.serve(w, r, a.exchange)
		}
	case a.cfg.Web != nil && strings.HasPrefix(p, "/assets/"):
		if methodAllowed(w, r, http.MethodGet) {
			serveFile(w, r, a.cfg.Web, "assets/"+strings.TrimPrefix(p, "/assets/"))
		}
	default:
		http.NotFound(w, r)
	}
}

func (a *App) sessionRoute(w http.ResponseWriter, r *http.Request, sid string, rest []string) {
	switch {
	case len(rest) == 1 && rest[0] == "messages":
		if methodAllowed(w, r, http.MethodPost) {
			a.serve(w, r, func(w http.ResponseWriter, r *http.Request) error { return a.messages(w, r, sid) })
		}
	case len(rest) == 1 && rest[0] == "cancel":
		if methodAllowed(w, r, http.MethodPost) {
			a.serve(w, r, func(w http.ResponseWriter, r *http.Request) error { return a.cancel(w, r, sid) })
		}
	case len(rest) == 1 && rest[0] == "diff":
		if methodAllowed(w, r, http.MethodGet) {
			a.serve(w, r, func(w http.ResponseWriter, r *http.Request) error { return a.diff(w, r, sid) })
		}
	case len(rest) == 2 && rest[0] == "permissions" && rest[1] != "":
		if methodAllowed(w, r, http.MethodPost) {
			a.serve(w, r, func(w http.ResponseWriter, r *http.Request) error { return a.permission(w, r, sid, rest[1]) })
		}
	default:
		// The events route only accepts WebSocket upgrades; plain HTTP finds no route.
		http.NotFound(w, r)
	}
}

func reject(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(`{"error":"WebSocket rejected"}`))
}

// readBody is server.py body(): application/json, at most maxSize bytes, a JSON
// object with only allowed keys.
func readBody(r *http.Request, allowed []string, maxSize int) (map[string]any, error) {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		return nil, requestErr("Expected application/json")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, int64(maxSize)+1))
	if err != nil {
		return nil, requestErr("Invalid JSON body")
	}
	if len(data) > maxSize {
		return nil, &BodyTooLargeError{Limit: maxSize}
	}
	var value any
	if !utf8.Valid(data) || json.Unmarshal(data, &value) != nil {
		return nil, requestErr("Invalid JSON body")
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, requestErr("Unexpected request fields")
	}
	for k := range obj {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			return nil, requestErr("Unexpected request fields")
		}
	}
	return obj, nil
}

func (a *App) projects(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		list, err := a.Store.Projects()
		if err != nil {
			return err
		}
		writeJSON(w, 200, list)
		return nil
	}
	data, err := readBody(r, []string{"path"}, maxBodyBytes)
	if err != nil {
		return err
	}
	project, err := a.Store.AddProject(data["path"])
	if err != nil {
		return err
	}
	writeJSON(w, 201, project)
	return nil
}

// valueOr is Python's dict.get(key, default): a present null stays nil.
func valueOr(data map[string]any, key string, fallback any) any {
	if v, ok := data[key]; ok {
		return v
	}
	return fallback
}

func (a *App) sessions(w http.ResponseWriter, r *http.Request, pid string) error {
	if r.Method != http.MethodPost {
		list, err := a.Store.Sessions(pid)
		if err != nil {
			return err
		}
		writeJSON(w, 200, list)
		return nil
	}
	data, err := readBody(r, []string{"player", "model", "permission", "title"}, maxBodyBytes)
	if err != nil {
		return err
	}
	defaults, err := a.Store.Settings()
	if err != nil {
		return err
	}
	project, err := a.Store.Project(pid)
	if err != nil {
		return err
	}
	head, _, err := gitRead(project.Path, 128, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	var base *string
	if head != nil && strings.TrimSpace(*head) != "" {
		b := pyStrip(*head)
		base = &b
	}
	session, err := a.Store.CreateSession(pid, valueOr(data, "player", defaults.Get("default_player")),
		data["model"], valueOr(data, "permission", defaults.Get("permission")),
		valueOr(data, "title", "New session"), base)
	if err != nil {
		return err
	}
	writeJSON(w, 201, session)
	return nil
}

func (a *App) messages(w http.ResponseWriter, r *http.Request, sid string) error {
	data, err := readBody(r, []string{"prompt"}, maxBodyBytes)
	if err != nil {
		return err
	}
	if err := a.Manager.Start(sid, data["prompt"]); err != nil {
		return err
	}
	session, err := a.Store.Session(sid)
	if err != nil {
		return err
	}
	if session.Title == "New session" {
		title := truncateRunes(firstLine(pyStrip(data["prompt"].(string))), 60)
		if err := a.Store.UpdateSession(sid, map[string]any{"title": title}); err != nil {
			return err
		}
	}
	writeJSON(w, 202, orderedObject{{"session_id", sid}, {"status", "running"}})
	return nil
}

func (a *App) pickFolder(w http.ResponseWriter, r *http.Request) error {
	if _, err := readBody(r, nil, maxBodyBytes); err != nil {
		return err
	}
	if a.cfg.FolderPicker == nil {
		return requestErr("Native folder picker is unavailable; enter the folder path.")
	}
	path, err := a.cfg.FolderPicker(r.Context())
	if err != nil {
		return err
	}
	var result any
	if path != "" {
		result = path
	}
	writeJSON(w, 200, map[string]any{"path": result})
	return nil
}

func (a *App) cancel(w http.ResponseWriter, r *http.Request, sid string) error {
	data := map[string]any{}
	if r.Header.Get("Content-Type") != "" {
		var err error
		if data, err = readBody(r, []string{"confirm_stopped"}, maxBodyBytes); err != nil {
			return err
		}
	}
	if _, err := a.Store.Session(sid); err != nil {
		return err
	}
	confirm, ok := valueOr(data, "confirm_stopped", false).(bool)
	if !ok {
		return requestErr("confirm_stopped must be boolean")
	}
	if err := a.Manager.Cancel(sid, confirm); err != nil {
		return err
	}
	session, err := a.Store.Session(sid)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]string{"status": session.Status})
	return nil
}

func (a *App) permission(w http.ResponseWriter, r *http.Request, sid, rid string) error {
	data, err := readBody(r, []string{"allow"}, maxBodyBytes)
	if err != nil {
		return err
	}
	if _, err := a.Store.Session(sid); err != nil {
		return err
	}
	allow, ok := data["allow"].(bool)
	if !ok {
		return requestErr("allow must be boolean")
	}
	if err := a.Manager.Answer(sid, rid, allow); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
	return nil
}

func (a *App) settings(w http.ResponseWriter, r *http.Request) error {
	if r.Method == http.MethodPut {
		data, err := readBody(r, []string{"default_player", "permission"}, maxBodyBytes)
		if err != nil {
			return err
		}
		if _, err := a.Store.SetSettings(data); err != nil {
			return err
		}
	}
	values, err := a.Store.Settings()
	if err != nil {
		return err
	}
	present := false
	if a.cfg.KeyPresent != nil {
		present = a.cfg.KeyPresent()
	}
	writeJSON(w, 200, append(values, member{"openrouter_key_present", present}))
	return nil
}

func (a *App) players(w http.ResponseWriter, r *http.Request) error {
	var health []PlayerHealth
	if a.cfg.PlayerHealth != nil {
		health = a.cfg.PlayerHealth(r.Context())
	}
	catalog := []any{}
	var catalogErr any
	if a.cfg.Models != nil {
		models, err := a.cfg.Models(r.Context())
		if err != nil {
			catalogErr = err.Error()
		} else if models != nil {
			catalog = models
		}
	}
	out := make([]orderedObject, 0, len(health))
	for _, h := range health {
		models, modelsErr := any([]any{}), any(nil)
		if h.ID == "openrouter" {
			models, modelsErr = catalog, catalogErr
		}
		out = append(out, orderedObject{
			{"id", h.ID}, {"name", h.Name}, {"path", h.Path}, {"version", h.Version},
			{"logged_in", h.LoggedIn}, {"fix", h.Fix}, {"ready", h.Ready()},
			{"models", models}, {"models_error", modelsErr},
		})
	}
	writeJSON(w, 200, out)
	return nil
}

func (a *App) diff(w http.ResponseWriter, r *http.Request, sid string) error {
	session, err := a.Store.Session(sid)
	if err != nil {
		return err
	}
	if session.BaseCommit == nil || *session.BaseCommit == "" {
		writeJSON(w, 200, orderedObject{{"base_commit", nil}, {"diff", ""}, {"untracked", []string{}}, {"truncated", false}})
		return nil
	}
	cwd, err := a.Manager.folderOf(session)
	if err != nil {
		return err
	}
	output, truncated, err := gitRead(cwd, gitOutputLimit, "diff", "--no-ext-diff", "--no-textconv", *session.BaseCommit, "--")
	if err != nil {
		return err
	}
	untracked, more, err := gitRead(cwd, gitOutputLimit, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	if output == nil {
		return requestErr("Git diff is unavailable; the saved base may no longer exist")
	}
	names := []string{}
	if untracked != nil {
		for _, name := range strings.Split(*untracked, "\x00") {
			if name != "" {
				names = append(names, name)
			}
		}
	}
	writeJSON(w, 200, orderedObject{{"base_commit", *session.BaseCommit}, {"diff", *output},
		{"untracked", names}, {"truncated", truncated || more}})
	return nil
}

func (a *App) exchange(w http.ResponseWriter, r *http.Request) error {
	if r.Header.Get("Origin") != a.origin {
		writeJSON(w, 403, map[string]string{"error": "Local browser Origin required"})
		return nil
	}
	data, err := readBody(r, []string{"nonce"}, maxBootstrapBytes)
	if err != nil {
		return err
	}
	if !a.Bootstrap.Consume(data["nonce"]) {
		writeJSON(w, 401, map[string]string{"error": "Bootstrap expired or already used. Reopen Orchestra from the launcher."})
		return nil
	}
	writeJSON(w, 200, map[string]string{"token": a.Token})
	return nil
}

// capWriter keeps the first limit+1 bytes and calls full() once it has more than limit.
type capWriter struct {
	limit     int
	buf       []byte
	truncated bool
	full      func()
}

func (c *capWriter) Write(p []byte) (int, error) {
	if !c.truncated {
		room := c.limit + 1 - len(c.buf)
		if room > len(p) {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
		if len(c.buf) > c.limit {
			c.truncated = true
			c.full()
		}
	}
	return len(p), nil // keep draining so the child never blocks on a full pipe
}

// gitRead runs a read-only git command in cwd (server.py git_read). Repository
// fsmonitor hooks and pagers are disabled, stdin is empty, stderr is discarded, output
// is capped at limit bytes (truncated=true), and it times out after 10 seconds.
// output is nil when git is missing or exits non-zero.
func gitRead(cwd string, limit int, args ...string) (output *string, truncated bool, err error) {
	timeout, stop := context.WithTimeout(context.Background(), gitTimeout)
	defer stop()
	run, kill := context.WithCancel(timeout)
	defer kill()
	argv := append([]string{"-c", "core.fsmonitor=false", "-c", "safe.bareRepository=explicit",
		"--no-pager", "--no-optional-locks", "-C", cwd}, args...)
	cmd := exec.CommandContext(run, "git", argv...)
	cmd.WaitDelay = 2 * time.Second
	out := &capWriter{limit: limit, full: kill}
	cmd.Stdout = out
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	waitErr := cmd.Wait()
	if out.truncated {
		s := decodeReplace(out.buf[:limit])
		return &s, true, nil
	}
	if errors.Is(timeout.Err(), context.DeadlineExceeded) {
		return nil, false, requestErr("Git inspection timed out")
	}
	if waitErr != nil {
		return nil, false, nil
	}
	s := decodeReplace(out.buf)
	return &s, false, nil
}

// events is the WebSocket journal stream: replay after ?after=<seq>, then live.
func (a *App) events(w http.ResponseWriter, r *http.Request, sid string) {
	a.wsMu.Lock()
	if a.wsClosed {
		a.wsMu.Unlock()
		reject(w)
		return
	}
	a.ws.Add(1)
	a.wsMu.Unlock()
	defer a.ws.Done()

	raw := "0"
	if values, ok := r.URL.Query()["after"]; ok {
		raw = values[0]
	}
	after, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		reject(w)
		return
	}
	sub, err := a.Manager.Subscribe(sid, after)
	if err != nil {
		reject(w)
		return
	}
	defer sub.Close()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{"orchestra"}, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Client messages are ignored; a read error means the client disconnected.
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()
	for {
		rec, err := sub.Next(ctx)
		if errors.Is(err, ErrSlowSubscriber) {
			conn.Close(websocket.StatusTryAgainLater, "Reconnect with last received seq")
			return
		}
		if err != nil {
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, []byte(rec.Payload)); err != nil {
			return
		}
	}
}

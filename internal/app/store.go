package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/Adeel0-0/agent-orchestra/internal/model"
	"github.com/Adeel0-0/agent-orchestra/internal/players"

	_ "modernc.org/sqlite" // pure-Go driver "sqlite"
)

// DefaultPlayers are the player ids the Python registry knows, in registry order.
var DefaultPlayers = []string{"codex", "antigravity", "claude", "openrouter"}

const schema = `
CREATE TABLE IF NOT EXISTS projects (
    id TEXT PRIMARY KEY, path TEXT UNIQUE NOT NULL, name TEXT NOT NULL, last_opened REAL NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id),
    player TEXT NOT NULL, model TEXT, title TEXT NOT NULL, worktree TEXT,
    backend_session_id TEXT, status TEXT NOT NULL, created REAL NOT NULL,
    updated REAL NOT NULL, permission TEXT NOT NULL, base_commit TEXT);
CREATE TABLE IF NOT EXISTS events (
    session_id TEXT NOT NULL REFERENCES sessions(id), seq INTEGER NOT NULL,
    payload TEXT NOT NULL, PRIMARY KEY(session_id,seq));
CREATE TABLE IF NOT EXISTS player_states (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id), payload TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

// Project is one row of the projects table. JSON keys follow the column order.
type Project struct {
	ID         string  `json:"id"`
	Path       string  `json:"path"`
	Name       string  `json:"name"`
	LastOpened float64 `json:"last_opened"`
}

// Session is one row of the sessions table. Nullable columns are pointers.
type Session struct {
	ID               string  `json:"id"`
	ProjectID        string  `json:"project_id"`
	Player           string  `json:"player"`
	Model            *string `json:"model"`
	Title            string  `json:"title"`
	Worktree         *string `json:"worktree"`
	BackendSessionID *string `json:"backend_session_id"`
	Status           string  `json:"status"`
	Created          float64 `json:"created"`
	Updated          float64 `json:"updated"`
	Permission       string  `json:"permission"`
	BaseCommit       *string `json:"base_commit"`
}

// BlockedSession is a session whose project needs recovery, with its project path.
type BlockedSession struct {
	Session
	Path string `json:"path"`
}

// Record is one journal event: the stored JSON payload and its sequence number.
type Record struct {
	Seq     int64
	Type    string
	Payload string
}

// MarshalJSON sends the stored payload unchanged.
func (r Record) MarshalJSON() ([]byte, error) { return []byte(r.Payload), nil }

// Fields decodes the payload.
func (r Record) Fields() map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(r.Payload), &m)
	return m
}

// StoreOptions configure OpenStore.
type StoreOptions struct {
	// Players are the known player ids; nil means DefaultPlayers.
	Players []string
}

// Store is the SQLite journal with atomic player snapshots and repairable JSONL
// transcripts. One server process owns a data directory at a time.
type Store struct {
	root        string
	transcripts string
	players     []string

	mu       sync.Mutex // serializes every database use, like Python's RLock
	db       *sql.DB
	conn     *sql.Conn
	lockFile *os.File
	closed   bool
}

// DefaultDataDir is ~/.orchestra.
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".orchestra"), nil
}

// OpenStore opens (creating if needed) the data directory root ("" = ~/.orchestra),
// takes the server.lock OS lock, marks turns interrupted by an abrupt stop and repairs
// transcript mirrors.
func OpenStore(root string, opts StoreOptions) (*Store, error) {
	if root == "" {
		var err error
		if root, err = DefaultDataDir(); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	// Transcripts hold file contents and command output: keep an existing folder
	// private too (no effect on Windows ACLs).
	if runtime.GOOS != "windows" {
		if err := os.Chmod(root, 0o700); err != nil {
			return nil, err
		}
	}
	s := &Store{root: root, transcripts: filepath.Join(root, "sessions"), players: opts.Players}
	if s.players == nil {
		s.players = DefaultPlayers
	}
	if err := os.Mkdir(s.transcripts, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := s.takeServerLock(); err != nil {
		return nil, err
	}
	if err := s.openDB(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.recover(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) takeServerLock() error {
	f, err := os.OpenFile(filepath.Join(s.root, "server.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	// Python writes one byte so there is a byte 0 to lock. Reading it fails while
	// another process holds the Windows byte-range lock.
	first := make([]byte, 1)
	n, err := f.ReadAt(first, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		f.Close()
		return ErrServerOwned
	}
	if n == 0 {
		if _, err := f.WriteAt([]byte("0"), 0); err != nil {
			f.Close()
			return ErrServerOwned
		}
	}
	if err := lockServerFile(f); err != nil {
		f.Close()
		return ErrServerOwned
	}
	s.lockFile = f
	return nil
}

func (s *Store) openDB() error {
	db, err := sql.Open("sqlite", filepath.Join(s.root, "orchestra.db"))
	if err != nil {
		return err
	}
	s.db = db
	// One connection for the life of the store, like Python's single sqlite3 connection.
	conn, err := db.Conn(context.Background())
	if err != nil {
		return err
	}
	s.conn = conn
	for _, stmt := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", schema} {
		if _, err := conn.ExecContext(context.Background(), stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) recover() error {
	type row struct{ id, status string }
	var rows []row
	err := s.query(func(c *sql.Conn) error {
		r, err := c.QueryContext(context.Background(), "SELECT id,status FROM sessions")
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var x row
			if err := r.Scan(&x.id, &x.status); err != nil {
				return err
			}
			rows = append(rows, x)
		}
		return r.Err()
	})
	if err != nil {
		return err
	}
	for _, x := range rows {
		switch x.status {
		case "running", "waiting_permission", "cancelling":
			ev := map[string]any{"type": players.TurnCompleted, "stop_reason": "interrupted"}
			if _, err := s.Append(x.id, ev, nil); err != nil {
				return err
			}
		}
		if err := s.repair(x.id); err != nil {
			return err
		}
	}
	return nil
}

// Close releases the database and the server lock. The lock file itself remains.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var first error
	if s.conn != nil {
		first = s.conn.Close()
	}
	if s.db != nil {
		if err := s.db.Close(); err != nil && first == nil {
			first = err
		}
	}
	if s.lockFile != nil {
		// Closing the descriptor releases the OS lock; never remove a live lock file.
		s.lockFile.Close()
	}
	return first
}

// Root is the data directory.
func (s *Store) Root() string { return s.root }

// TranscriptPath is sessions/<sid>.jsonl.
func (s *Store) TranscriptPath(sid string) string {
	return filepath.Join(s.transcripts, sid+".jsonl")
}

func (s *Store) query(fn func(*sql.Conn) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("store is closed")
	}
	return fn(s.conn)
}

func (s *Store) tx(fn func(*sql.Tx) error) error {
	return s.query(func(c *sql.Conn) error {
		tx, err := c.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			tx.Rollback()
			return err
		}
		return tx.Commit()
	})
}

type scanner interface{ Scan(dest ...any) error }

const sessionColumns = "id,project_id,player,model,title,worktree,backend_session_id,status,created,updated,permission,base_commit"

func scanSession(r scanner, extra ...any) (Session, error) {
	var x Session
	var m, w, b, bc sql.NullString
	dest := append([]any{&x.ID, &x.ProjectID, &x.Player, &m, &x.Title, &w, &b, &x.Status,
		&x.Created, &x.Updated, &x.Permission, &bc}, extra...)
	if err := r.Scan(dest...); err != nil {
		return x, err
	}
	x.Model, x.Worktree, x.BackendSessionID, x.BaseCommit = nullable(m), nullable(w), nullable(b), nullable(bc)
	return x, nil
}

func nullable(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

// Projects lists projects, most recently opened first.
func (s *Store) Projects() ([]Project, error) {
	out := []Project{}
	err := s.query(func(c *sql.Conn) error {
		r, err := c.QueryContext(context.Background(), "SELECT id,path,name,last_opened FROM projects ORDER BY last_opened DESC")
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var p Project
			if err := r.Scan(&p.ID, &p.Path, &p.Name, &p.LastOpened); err != nil {
				return err
			}
			out = append(out, p)
		}
		return r.Err()
	})
	return out, err
}

// Project returns one project or a NotFoundError.
func (s *Store) Project(pid string) (Project, error) {
	var p Project
	err := s.query(func(c *sql.Conn) error {
		return c.QueryRowContext(context.Background(), "SELECT id,path,name,last_opened FROM projects WHERE id=?", pid).
			Scan(&p.ID, &p.Path, &p.Name, &p.LastOpened)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return p, notFound("Unknown project")
	}
	return p, err
}

// AddProject registers an existing folder (or refreshes its last_opened). path is
// any so JSON values are validated here with the Python messages.
func (s *Store) AddProject(path any) (Project, error) {
	str, ok := path.(string)
	if !ok || pyStrip(str) == "" {
		return Project{}, requestErr("path must be a non-empty folder path")
	}
	folder, err := resolveStrict(str)
	if err != nil {
		return Project{}, requestErr("Project folder does not exist")
	}
	info, err := os.Stat(folder)
	if err != nil {
		return Project{}, requestErr("Project folder does not exist")
	}
	if !info.IsDir() {
		return Project{}, requestErr("Project path must be a folder")
	}
	normalized := normcase(folder)
	name := filepath.Base(folder)
	if name == "" || name == "." || name == string(filepath.Separator) || strings.HasSuffix(folder, string(filepath.Separator)) {
		name = folder // a drive or file-system root has no name
	}
	var p Project
	err = s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO projects VALUES (?,?,?,?)
			ON CONFLICT(path) DO UPDATE SET last_opened=excluded.last_opened`,
			model.RandomHex(16), normalized, name, model.Now()); err != nil {
			return err
		}
		return tx.QueryRow("SELECT id,path,name,last_opened FROM projects WHERE path=?", normalized).
			Scan(&p.ID, &p.Path, &p.Name, &p.LastOpened)
	})
	return p, err
}

func (s *Store) knownPlayer(id string) bool {
	for _, p := range s.players {
		if p == id {
			return true
		}
	}
	return false
}

// CreateSession validates and inserts a new idle session. Arguments other than pid
// and baseCommit are any so JSON values are validated here with the Python messages;
// model and title may be nil (title nil means "New session").
func (s *Store) CreateSession(pid string, player, mdl, permission, title any, baseCommit *string) (Session, error) {
	if _, err := s.Project(pid); err != nil {
		return Session{}, err
	}
	playerID, ok := player.(string)
	if !ok || !s.knownPlayer(playerID) {
		return Session{}, requestErr("Unknown player")
	}
	perm, ok := permission.(string)
	if !ok || !players.ValidPermission(perm) {
		return Session{}, requestErr("Unknown permission mode")
	}
	var modelValue *string
	if mdl != nil {
		m, ok := mdl.(string)
		if !ok || pyStrip(m) == "" || pyLen(m) > 256 {
			return Session{}, requestErr("model must be a non-empty string or null")
		}
		modelValue = &m
	}
	if playerID == "openrouter" && modelValue == nil {
		return Session{}, requestErr("OpenRouter requires a model")
	}
	if title == nil {
		title = "New session"
	}
	t, ok := title.(string)
	if !ok || pyStrip(t) == "" || pyLen(t) > 200 {
		return Session{}, requestErr("title must contain 1 to 200 characters")
	}
	sid, now := model.RandomHex(16), model.Now()
	err := s.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO sessions VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
			sid, pid, playerID, modelValue, t, nil, nil, "idle", now, now, perm, baseCommit)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return s.Session(sid)
}

// Session returns one session or a NotFoundError.
func (s *Store) Session(sid string) (Session, error) {
	var x Session
	err := s.query(func(c *sql.Conn) error {
		var err error
		x, err = scanSession(c.QueryRowContext(context.Background(), "SELECT "+sessionColumns+" FROM sessions WHERE id=?", sid))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return x, notFound("Unknown session")
	}
	return x, err
}

// Sessions lists a project's sessions, most recently updated first.
func (s *Store) Sessions(pid string) ([]Session, error) {
	if _, err := s.Project(pid); err != nil {
		return nil, err
	}
	out := []Session{}
	err := s.query(func(c *sql.Conn) error {
		r, err := c.QueryContext(context.Background(), "SELECT "+sessionColumns+" FROM sessions WHERE project_id=? ORDER BY updated DESC", pid)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			x, err := scanSession(r)
			if err != nil {
				return err
			}
			out = append(out, x)
		}
		return r.Err()
	})
	return out, err
}

var updatableSessionFields = map[string]bool{"status": true, "title": true, "backend_session_id": true, "base_commit": true}

// UpdateSession sets status, title, backend_session_id and/or base_commit, and updated.
func (s *Store) UpdateSession(sid string, values map[string]any) error {
	if _, err := s.Session(sid); err != nil {
		return err
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		if !updatableSessionFields[k] {
			return requestErr("Unsupported session update")
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sets := make([]string, 0, len(keys)+1)
	args := make([]any, 0, len(keys)+2)
	for _, k := range keys {
		sets = append(sets, k+"=?") // k comes from the fixed whitelist above
		args = append(args, values[k])
	}
	sets = append(sets, "updated=?")
	args = append(args, model.Now(), sid)
	return s.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE sessions SET "+strings.Join(sets, ",")+" WHERE id=?", args...)
		return err
	})
}

var terminalStatus = map[string]string{"end_turn": "idle", "cancelled": "cancelled", "interrupted": "interrupted"}

// Append journals one event with the next seq, optionally with the player's state
// snapshot, in one transaction. A session.started event records the backend session
// id, and message.user / turn.completed set the session status atomically with the
// event. The JSONL mirror is written afterwards; a failed mirror write is repaired on
// the next startup.
func (s *Store) Append(sid string, event map[string]any, state map[string]any) (Record, error) {
	if _, err := s.Session(sid); err != nil {
		return Record{}, err
	}
	typ, _ := event["type"].(string)
	var statePayload []byte
	if state != nil {
		var err error
		if statePayload, err = pyDumps(state); err != nil {
			return Record{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Record{}, errors.New("store is closed")
	}
	var rec Record
	tx, err := s.conn.BeginTx(context.Background(), nil)
	if err != nil {
		return rec, err
	}
	err = func() error {
		var seq int64
		if err := tx.QueryRow("SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE session_id=?", sid).Scan(&seq); err != nil {
			return err
		}
		payload, err := encodeRecord(event, seq, model.Now())
		if err != nil {
			return err
		}
		rec = Record{Seq: seq, Type: typ, Payload: payload}
		if _, err := tx.Exec("INSERT INTO events VALUES (?,?,?)", sid, seq, payload); err != nil {
			return err
		}
		if statePayload != nil {
			if _, err := tx.Exec(`INSERT INTO player_states VALUES (?,?)
				ON CONFLICT(session_id) DO UPDATE SET payload=excluded.payload`, sid, string(statePayload)); err != nil {
				return err
			}
		}
		if typ == players.SessionStarted {
			if id, ok := event["backend_session_id"].(string); ok && id != "" {
				if _, err := tx.Exec("UPDATE sessions SET backend_session_id=? WHERE id=?", id, sid); err != nil {
					return err
				}
			}
		}
		if typ == players.TurnCompleted || typ == "message.user" {
			status := "running"
			if typ == players.TurnCompleted {
				reason, _ := event["stop_reason"].(string)
				if status = terminalStatus[reason]; status == "" {
					status = "error"
				}
			}
			if _, err := tx.Exec("UPDATE sessions SET status=?,updated=? WHERE id=?", status, model.Now(), sid); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		tx.Rollback()
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, err
	}
	if err := appendLine(s.TranscriptPath(sid), rec.Payload); err != nil {
		log.Printf("orchestra app: transcript mirror write failed; durable journal retained for session %s", sid)
	}
	return rec, nil
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(line + "\n")
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// Events returns up to limit records with seq > after.
func (s *Store) Events(sid string, after int64, limit int) ([]Record, error) {
	if _, err := s.Session(sid); err != nil {
		return nil, err
	}
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, requestErr("Invalid event offset or page size")
	}
	out := []Record{}
	err := s.query(func(c *sql.Conn) error {
		r, err := c.QueryContext(context.Background(),
			"SELECT seq,payload FROM events WHERE session_id=? AND seq>? ORDER BY seq LIMIT ?", sid, after, limit)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var rec Record
			if err := r.Scan(&rec.Seq, &rec.Payload); err != nil {
				return err
			}
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(rec.Payload), &head)
			rec.Type = head.Type
			out = append(out, rec)
		}
		return r.Err()
	})
	return out, err
}

// LastSeq is the highest stored seq (0 when none).
func (s *Store) LastSeq(sid string) (int64, error) {
	if _, err := s.Session(sid); err != nil {
		return 0, err
	}
	var seq int64
	err := s.query(func(c *sql.Conn) error {
		return c.QueryRowContext(context.Background(), "SELECT COALESCE(MAX(seq),0) FROM events WHERE session_id=?", sid).Scan(&seq)
	})
	return seq, err
}

// BlockedSessions are sessions in cleanup_failed or interrupted state, with project paths.
func (s *Store) BlockedSessions() ([]BlockedSession, error) {
	var out []BlockedSession
	err := s.query(func(c *sql.Conn) error {
		cols := strings.ReplaceAll("sessions."+sessionColumns, ",", ",sessions.")
		r, err := c.QueryContext(context.Background(), "SELECT "+cols+`, projects.path FROM sessions
			JOIN projects ON projects.id=sessions.project_id WHERE status IN ('cleanup_failed','interrupted')`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var b BlockedSession
			x, err := scanSession(r, &b.Path)
			if err != nil {
				return err
			}
			b.Session = x
			out = append(out, b)
		}
		return r.Err()
	})
	return out, err
}

// PlayerState is the saved player snapshot, or nil.
func (s *Store) PlayerState(sid string) (map[string]any, error) {
	if _, err := s.Session(sid); err != nil {
		return nil, err
	}
	var payload string
	err := s.query(func(c *sql.Conn) error {
		return c.QueryRowContext(context.Background(), "SELECT payload FROM player_states WHERE session_id=?", sid).Scan(&payload)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(payload), &state); err != nil {
		return nil, err
	}
	return state, nil
}

// repair rewrites sessions/<sid>.jsonl from the journal when its size disagrees.
func (s *Store) repair(sid string) error {
	return s.query(func(c *sql.Conn) error {
		var expected int64
		if err := c.QueryRowContext(context.Background(),
			"SELECT COALESCE(SUM(length(CAST(payload AS BLOB))+1),0) FROM events WHERE session_id=?", sid).Scan(&expected); err != nil {
			return err
		}
		target := s.TranscriptPath(sid)
		if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() && info.Size() == expected {
			return nil
		}
		r, err := c.QueryContext(context.Background(), "SELECT payload FROM events WHERE session_id=? ORDER BY seq", sid)
		if err != nil {
			return err
		}
		defer r.Close()
		temporary := filepath.Join(s.transcripts, sid+".tmp")
		out, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		for r.Next() {
			var payload string
			if err := r.Scan(&payload); err != nil {
				out.Close()
				return err
			}
			if _, err := out.WriteString(payload + "\n"); err != nil {
				out.Close()
				return err
			}
		}
		if err := out.Close(); err != nil {
			return err
		}
		if err := r.Err(); err != nil {
			return err
		}
		return os.Rename(temporary, target)
	})
}

// Settings returns default_player and permission (with defaults) plus any other
// stored keys, in that order.
func (s *Store) Settings() (orderedObject, error) {
	stored := map[string]any{}
	err := s.query(func(c *sql.Conn) error {
		r, err := c.QueryContext(context.Background(), "SELECT key,value FROM settings")
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var k, v string
			if err := r.Scan(&k, &v); err != nil {
				return err
			}
			var value any
			if err := json.Unmarshal([]byte(v), &value); err != nil {
				return fmt.Errorf("setting %q: %w", k, err)
			}
			stored[k] = value
		}
		return r.Err()
	})
	if err != nil {
		return nil, err
	}
	result := orderedObject{{"default_player", "codex"}, {"permission", "edit"}}
	for i := range result {
		if v, ok := stored[result[i].Key]; ok {
			result[i].Value = v
			delete(stored, result[i].Key)
		}
	}
	extra := make([]string, 0, len(stored))
	for k := range stored {
		extra = append(extra, k)
	}
	sort.Strings(extra)
	for _, k := range extra {
		result = append(result, member{k, stored[k]})
	}
	return result, nil
}

// Get returns one value by key, or nil.
func (o orderedObject) Get(key string) any {
	for _, m := range o {
		if m.Key == key {
			return m.Value
		}
	}
	return nil
}

// SetSettings stores default_player and/or permission. Credentials are never settings.
func (s *Store) SetSettings(values map[string]any) (orderedObject, error) {
	for k := range values {
		if k != "default_player" && k != "permission" {
			return nil, requestErr("Settings accept only default_player and permission; credentials use the OS keyring")
		}
	}
	player, ok := values["default_player"]
	if !ok {
		player = "codex"
	}
	permission, ok := values["permission"]
	if !ok {
		permission = "edit"
	}
	p, pok := player.(string)
	m, mok := permission.(string)
	if !pok || !s.knownPlayer(p) || !mok || !players.ValidPermission(m) {
		return nil, requestErr("Unknown player or permission mode")
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	err := s.tx(func(tx *sql.Tx) error {
		for _, k := range keys {
			v, err := pyDumps(values[k])
			if err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT INTO settings VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", k, string(v)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Settings()
}

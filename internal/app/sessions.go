package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Adeel0-0/agent-orchestra/internal/bus"
	"github.com/Adeel0-0/agent-orchestra/internal/model"
	"github.com/Adeel0-0/agent-orchestra/internal/players"
)

const (
	maxPromptBytes     = 256 * 1024
	writerLeaseSeconds = 120
	blockedLeaseSecond = 86400
	subscriberBuffer   = 128
	defaultHeartbeat   = 30 * time.Second
)

// Factory builds the player for a session's next turn in cwd. The server never
// imports the player registry; the caller supplies this.
type Factory func(session Session, cwd string) (players.Player, error)

// Canceller is an optional player hook called after every turn ends, like Python's
// player.cancel(). An error means the backend's processes may still be running: the
// session becomes cleanup_failed and its project stays blocked until the user
// confirms the processes are stopped.
type Canceller interface {
	Cancel() error
}

// leaseLock is the part of bus.LockManager the manager renews and releases.
type leaseLock interface {
	Heartbeat(agentID string, extendSeconds int) (bool, error)
	Release(agentID string, force bool) (bool, error)
}

type lease struct {
	lock  leaseLock
	owner string
}

type turn struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex // orders status writes from Cancel/Answer before the turn's final status
	ended bool
}

func (t *turn) running() bool {
	select {
	case <-t.done:
		return false
	default:
		return true
	}
}

// Manager owns agent turns independently of HTTP requests and WebSocket subscribers:
// one turn per session, no concurrent turns in overlapping folders, and the engine
// writer lock claimed, renewed and released around every turn (no handoff event).
type Manager struct {
	store          *Store
	factory        Factory
	launch         string
	heartbeatEvery time.Duration

	startMu sync.Mutex // serializes Start and recovery so the overlap check is atomic
	pubMu   sync.Mutex // one publish at a time: journal order == delivery order
	mu      sync.Mutex // guards the maps below; never held while calling out

	closed      bool
	players     map[string]players.Player
	turns       map[string]*turn
	subs        map[string]map[*Subscription]struct{}
	paths       map[string]string
	permissions map[string]map[string]bool
	leases      map[string]lease
	snapRev     map[string]int64
}

// NewManager creates a session manager over store.
func NewManager(store *Store, factory Factory) *Manager {
	return &Manager{
		store: store, factory: factory, launch: model.RandomHex(16), heartbeatEvery: defaultHeartbeat,
		players: map[string]players.Player{}, turns: map[string]*turn{},
		subs: map[string]map[*Subscription]struct{}{}, paths: map[string]string{},
		permissions: map[string]map[string]bool{}, leases: map[string]lease{}, snapRev: map[string]int64{},
	}
}

func (m *Manager) snapshot(sid string) (map[string]any, int64, bool) {
	m.mu.Lock()
	player := m.players[sid]
	last, seen := m.snapRev[sid]
	m.mu.Unlock()
	sp, ok := player.(players.Snapshotter)
	if !ok {
		return nil, 0, false
	}
	rev := sp.SnapshotRevision()
	if seen && last == rev {
		return nil, 0, false
	}
	return sp.SnapshotState(), rev, true
}

// Publish journals event (with a player snapshot when its revision changed) and
// delivers the record to subscribers. A subscriber whose buffer is full is dropped
// and told to reconnect; it can never stall the turn.
func (m *Manager) Publish(sid string, event map[string]any) (Record, error) {
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	state, rev, snap := m.snapshot(sid)
	rec, err := m.store.Append(sid, event, state)
	if err != nil {
		return rec, err
	}
	m.mu.Lock()
	if snap && state != nil {
		if _, alive := m.players[sid]; alive {
			m.snapRev[sid] = rev
		}
	}
	var targets []*Subscription
	for sub := range m.subs[sid] {
		targets = append(targets, sub)
	}
	for _, sub := range targets {
		select {
		case sub.ch <- rec:
		default:
			delete(m.subs[sid], sub)
			sub.drop()
		}
	}
	m.mu.Unlock()
	return rec, nil
}

func (m *Manager) publishQuiet(sid string, event map[string]any) {
	if _, err := m.Publish(sid, event); err != nil {
		log.Printf("orchestra app: journal write failed for session %s: %v", sid, err)
	}
}

func (m *Manager) folderOf(session Session) (string, error) {
	if session.Worktree != nil && *session.Worktree != "" {
		return *session.Worktree, nil
	}
	p, err := m.store.Project(session.ProjectID)
	if err != nil {
		return "", err
	}
	return p.Path, nil
}

// Start validates and begins one turn. It returns as soon as the turn is running;
// the turn belongs to the manager, not the caller. prompt is any so JSON values are
// validated here with the Python message.
func (m *Manager) Start(sid string, prompt any) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return conflictErr("Session manager is shutting down")
	}
	text, ok := prompt.(string)
	if !ok || pyStrip(text) == "" || len(text) > maxPromptBytes {
		return requestErr("prompt must contain 1 to 262144 UTF-8 bytes")
	}
	session, err := m.store.Session(sid)
	if err != nil {
		return err
	}
	m.mu.Lock()
	t := m.turns[sid]
	m.mu.Unlock()
	if t != nil && t.running() {
		return conflictErr("A turn is already running in this session")
	}
	folder, err := m.folderOf(session)
	if err != nil {
		return err
	}
	cwd, err := resolveStrict(folder)
	if err != nil {
		return requestErr("Project folder is missing or inaccessible")
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return requestErr("Project path is no longer a folder")
	}
	if err := m.checkOverlap(cwd); err != nil {
		return err
	}
	lock, err := bus.NewLockManager(filepath.Join(cwd, ".orchestra"))
	if err != nil {
		return err
	}
	owner := "session-" + sid + "-" + m.launch
	acquired, err := lock.Acquire(sid, owner, "local app turn", writerLeaseSeconds, false)
	if err != nil {
		return err
	}
	if !acquired {
		return conflictErr("The project writer lock is held by another agent")
	}
	started := false
	defer func() {
		if !started {
			lock.Release(owner, false)
		}
	}()
	player, err := m.playerFor(sid, session, cwd)
	if err != nil {
		return err
	}
	if _, err := m.Publish(sid, map[string]any{"type": "message.user", "text": text}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	t = &turn{cancel: cancel, done: make(chan struct{})}
	ls := lease{lock: lock, owner: owner}
	m.mu.Lock()
	m.paths[sid] = cwd
	m.leases[sid] = ls
	m.turns[sid] = t
	m.mu.Unlock()
	started = true
	go m.run(ctx, sid, text, player, ls, t)
	return nil
}

func (m *Manager) checkOverlap(cwd string) error {
	blocked, err := m.store.BlockedSessions()
	if err != nil {
		return err
	}
	for _, b := range blocked {
		folder := b.Path
		if b.Worktree != nil && *b.Worktree != "" {
			folder = *b.Worktree
		}
		if overlaps(cwd, resolveLoose(folder)) {
			return conflictErr("Project has an interrupted turn or failed cleanup. Stop leftover processes, then confirm_stopped through the cancel endpoint.")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, path := range m.paths {
		if overlaps(cwd, path) {
			return conflictErr("Another session is running in this project or an overlapping folder")
		}
	}
	return nil
}

func (m *Manager) playerFor(sid string, session Session, cwd string) (players.Player, error) {
	m.mu.Lock()
	player := m.players[sid]
	m.mu.Unlock()
	if player != nil {
		return player, nil
	}
	if m.factory == nil {
		return nil, fmt.Errorf("no player factory configured")
	}
	player, err := m.factory(session, cwd)
	if err != nil {
		return nil, err
	}
	if sp, ok := player.(players.Snapshotter); ok {
		state, err := m.store.PlayerState(sid)
		if err != nil {
			return nil, err
		}
		if state != nil {
			if err := sp.RestoreState(state); err != nil {
				return nil, err
			}
		}
	}
	m.mu.Lock()
	m.players[sid] = player
	m.mu.Unlock()
	return player, nil
}

func (m *Manager) heartbeat(ctx context.Context, sid string, ls lease, stop func()) {
	ticker := time.NewTicker(m.heartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if renewLease(ls, writerLeaseSeconds) {
			continue
		}
		func() {
			defer func() { recover() }() // reporting must not prevent stopping the turn
			m.publishQuiet(sid, players.ErrorEvent("Project writer lease was lost; turn stopped.").ToMap())
		}()
		stop()
		return
	}
}

func renewLease(ls lease, seconds int) (renewed bool) {
	defer func() {
		if recover() != nil {
			renewed = false
		}
	}()
	ok, err := ls.lock.Heartbeat(ls.owner, seconds)
	return err == nil && ok
}

// turnOutcome collects what the player emitted during one turn.
type turnOutcome struct {
	mu        sync.Mutex
	finished  bool
	completed bool
	errored   bool
	status    string
	failure   error
}

func (m *Manager) run(ctx context.Context, sid, prompt string, player players.Player, ls lease, t *turn) {
	defer close(t.done)
	hbCtx, hbStop := context.WithCancel(context.Background())
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		m.heartbeat(hbCtx, sid, ls, t.cancel)
	}()

	runCtx, stopRun := context.WithCancel(ctx)
	out := &turnOutcome{status: "idle"}
	emit := func(e players.Event) { m.handleEvent(runCtx, stopRun, sid, e, out) }
	sendErr := safeSend(runCtx, player, prompt, emit)

	out.mu.Lock()
	out.finished = true
	out.mu.Unlock()
	cancelled := ctx.Err() != nil
	stopRun()
	t.mu.Lock()
	t.ended = true
	t.mu.Unlock()

	status := out.status
	switch {
	case out.failure != nil:
		status = "error"
		m.publishQuiet(sid, players.ErrorEvent("Turn failed: "+out.failure.Error()).ToMap())
	case cancelled:
		status = "cancelled"
	case sendErr != nil:
		status = "error"
		m.publishQuiet(sid, players.ErrorEvent("Turn failed: "+sendErr.Error()).ToMap())
	}

	cleanupErr := safeCleanup(player)
	hbStop()
	<-hbDone
	if cleanupErr != nil {
		status = "cleanup_failed"
	}
	if !out.completed {
		if status != "cancelled" && status != "cleanup_failed" {
			status = "error"
		}
		reason := "error"
		if status == "cancelled" {
			reason = "cancelled"
		}
		m.publishQuiet(sid, map[string]any{"type": players.TurnCompleted, "stop_reason": reason})
	}
	if err := m.store.UpdateSession(sid, map[string]any{"status": status}); err != nil {
		log.Printf("orchestra app: status update failed for session %s: %v", sid, err)
	}
	if status == "cleanup_failed" {
		m.publishQuiet(sid, players.ErrorEvent("Player cleanup failed; project is blocked until leftover processes are stopped and confirmed.").ToMap())
		m.mu.Lock()
		delete(m.permissions, sid)
		m.mu.Unlock()
		// Keep (and extend) the writer lease so no other agent writes over the leftovers.
		renewLease(ls, blockedLeaseSecond)
		return
	}
	m.release(sid)
}

// handleEvent is the turn's emit callback. Events after the turn was cancelled or
// after Send returned are dropped, like events after Python's task cancellation.
func (m *Manager) handleEvent(ctx context.Context, stop func(), sid string, e players.Event, out *turnOutcome) {
	out.mu.Lock()
	defer out.mu.Unlock()
	if out.finished || ctx.Err() != nil {
		return
	}
	switch e.Type {
	case players.PermissionRequested:
		if rid, ok := e.Data["request_id"].(string); ok {
			m.mu.Lock()
			if m.permissions[sid] == nil {
				m.permissions[sid] = map[string]bool{}
			}
			m.permissions[sid][rid] = true
			m.mu.Unlock()
		}
		if err := m.store.UpdateSession(sid, map[string]any{"status": "waiting_permission"}); err != nil {
			out.failure = err
			stop()
			return
		}
	case players.Error:
		out.errored = true
	case players.TurnCompleted:
		out.completed = true
		switch reason := e.Str("stop_reason"); {
		case reason == "error" || out.errored:
			out.status = "error"
		case reason == "cancelled":
			out.status = "cancelled"
		default:
			out.status = "idle"
		}
	}
	if _, err := m.Publish(sid, e.ToMap()); err != nil {
		out.failure = err
		stop()
	}
}

func safeSend(ctx context.Context, player players.Player, prompt string, emit func(players.Event)) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return player.Send(ctx, prompt, emit)
}

func safeCleanup(player players.Player) (err error) {
	c, ok := player.(Canceller)
	if !ok {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return c.Cancel()
}

func (m *Manager) release(sid string) {
	m.mu.Lock()
	delete(m.players, sid)
	delete(m.snapRev, sid)
	delete(m.permissions, sid)
	delete(m.paths, sid)
	ls, ok := m.leases[sid]
	delete(m.leases, sid)
	m.mu.Unlock()
	if ok {
		if _, err := ls.lock.Release(ls.owner, false); err != nil {
			log.Printf("orchestra app: writer lock release failed for session %s: %v", sid, err)
		}
	}
}

// Wait blocks until the session's current turn (if any) has fully ended.
func (m *Manager) Wait(sid string) {
	m.mu.Lock()
	t := m.turns[sid]
	m.mu.Unlock()
	if t != nil {
		<-t.done
	}
}

// Cancel stops the running turn and waits for it. With no running turn and a session
// left interrupted or cleanup_failed, confirmStopped=true records the user's
// confirmation that leftover processes are stopped and releases only this session's
// stale app writer lease.
func (m *Manager) Cancel(sid string, confirmStopped bool) error {
	session, err := m.store.Session(sid)
	if err != nil {
		return err
	}
	m.mu.Lock()
	t := m.turns[sid]
	m.mu.Unlock()
	running := t != nil && t.running()
	if !running && (session.Status == "cleanup_failed" || session.Status == "interrupted") {
		if !confirmStopped {
			return conflictErr("Stop leftover processes, then repeat cancel with confirm_stopped=true")
		}
		return m.confirmStopped(sid, session)
	}
	if !running {
		return nil
	}
	t.mu.Lock()
	if !t.ended {
		if err := m.store.UpdateSession(sid, map[string]any{"status": "cancelling"}); err != nil {
			t.mu.Unlock()
			return err
		}
		// Cancelling the context stops the player and kills its process tree.
		t.cancel()
	}
	t.mu.Unlock()
	<-t.done
	return nil
}

func (m *Manager) confirmStopped(sid string, session Session) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	folder, err := m.folderOf(session)
	if err != nil {
		return err
	}
	if info, err := os.Stat(folder); err == nil && info.IsDir() {
		lock, err := bus.NewLockManager(filepath.Join(folder, ".orchestra"))
		if err != nil {
			return err
		}
		current := lock.CurrentLock()
		agent, _ := current["agent_id"].(string)
		if current != nil && current["task_id"] == any(sid) && strings.HasPrefix(agent, "session-") {
			if _, err := lock.Release(agent, false); err != nil {
				return err
			}
		}
	}
	m.release(sid)
	if err := m.store.UpdateSession(sid, map[string]any{"status": "cancelled"}); err != nil {
		return err
	}
	_, err = m.Publish(sid, map[string]any{"type": players.Notice, "message": "User confirmed leftover processes are stopped."})
	return err
}

// Answer resolves a pending permission request of this session's running turn.
func (m *Manager) Answer(sid, rid string, allow bool) error {
	if _, err := m.store.Session(sid); err != nil {
		return err
	}
	m.mu.Lock()
	pending := m.permissions[sid][rid]
	player := m.players[sid]
	t := m.turns[sid]
	if pending && player != nil {
		delete(m.permissions[sid], rid)
	}
	m.mu.Unlock()
	if !pending || player == nil {
		return conflictErr("Permission request is not pending in this session")
	}
	if err := player.AnswerPermission(rid, allow); err != nil {
		m.mu.Lock()
		if m.players[sid] == player && m.permissions[sid] != nil {
			m.permissions[sid][rid] = true
		}
		m.mu.Unlock()
		return err
	}
	if t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		if !t.ended {
			if err := m.store.UpdateSession(sid, map[string]any{"status": "running"}); err != nil {
				return err
			}
		}
	}
	_, err := m.Publish(sid, map[string]any{"type": "permission.answered", "request_id": rid, "allow": allow})
	return err
}

// Subscription follows one session's journal: replay after a seq, then live records.
type Subscription struct {
	m       *Manager
	sid     string
	after   int64
	ch      chan Record
	slow    chan struct{}
	once    sync.Once
	pending []Record
	live    bool
}

func (s *Subscription) drop() { s.once.Do(func() { close(s.slow) }) }

// Subscribe starts following sid after seq "after" (0 replays everything).
func (m *Manager) Subscribe(sid string, after int64) (*Subscription, error) {
	latest, err := m.store.LastSeq(sid)
	if err != nil {
		return nil, err
	}
	if after < 0 || after > latest {
		return nil, requestErr("after must be a sequence between zero and the last stored event")
	}
	sub := &Subscription{m: m, sid: sid, after: after, ch: make(chan Record, subscriberBuffer), slow: make(chan struct{})}
	m.mu.Lock()
	if m.closed {
		sub.drop()
	} else {
		if m.subs[sid] == nil {
			m.subs[sid] = map[*Subscription]struct{}{}
		}
		m.subs[sid][sub] = struct{}{}
	}
	m.mu.Unlock()
	return sub, nil
}

// Next returns the next record. It returns ErrSlowSubscriber when the subscriber
// fell behind (or the manager closed); reconnect with the last received seq.
func (s *Subscription) Next(ctx context.Context) (Record, error) {
	for {
		if !s.live {
			if len(s.pending) == 0 {
				page, err := s.m.store.Events(s.sid, s.after, 1000)
				if err != nil {
					return Record{}, err
				}
				if len(page) == 0 {
					s.live = true
					continue
				}
				s.pending = page
			}
			rec := s.pending[0]
			s.pending = s.pending[1:]
			s.after = rec.Seq
			return rec, nil
		}
		select {
		case <-s.slow:
			return Record{}, ErrSlowSubscriber
		default:
		}
		select {
		case <-ctx.Done():
			return Record{}, ctx.Err()
		case <-s.slow:
			return Record{}, ErrSlowSubscriber
		case rec := <-s.ch:
			if rec.Seq > s.after {
				s.after = rec.Seq
				return rec, nil
			}
		}
	}
}

// Close stops delivery to this subscription.
func (s *Subscription) Close() {
	s.m.mu.Lock()
	delete(s.m.subs[s.sid], s)
	s.m.mu.Unlock()
}

// Close refuses new turns, cancels running ones (waiting for their cleanup) and
// disconnects subscribers.
func (m *Manager) Close() {
	m.startMu.Lock()
	m.mu.Lock()
	m.closed = true
	var running []string
	for sid, t := range m.turns {
		if t.running() {
			running = append(running, sid)
		}
	}
	m.mu.Unlock()
	m.startMu.Unlock()
	for _, sid := range running {
		if err := m.Cancel(sid, false); err != nil {
			log.Printf("orchestra app: cancelling session %s at shutdown: %v", sid, err)
		}
	}
	m.mu.Lock()
	for sid, subs := range m.subs {
		for sub := range subs {
			sub.drop()
		}
		delete(m.subs, sid)
	}
	m.mu.Unlock()
}

// resolveLoose is Path(p).resolve() without strict: it never fails.
func resolveLoose(p string) string {
	if r, err := resolveStrict(p); err == nil {
		return r
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// overlaps reports whether either folder contains the other (case-insensitive on Windows).
func overlaps(a, b string) bool { return within(a, b) || within(b, a) }

func within(child, parent string) bool {
	c, p := normcase(filepath.Clean(child)), normcase(filepath.Clean(parent))
	if c == p {
		return true
	}
	sep := normcase(string(filepath.Separator))
	if !strings.HasSuffix(p, sep) {
		p += sep
	}
	return strings.HasPrefix(c, p)
}

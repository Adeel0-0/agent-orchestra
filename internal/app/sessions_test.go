package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Adeel0-0/agent-orchestra/internal/bus"
	"github.com/Adeel0-0/agent-orchestra/internal/players"
)

type sessionFixture struct {
	t       *testing.T
	root    string
	project string
	data    string
	store   *Store
	player  *controlledPlayer
	manager *Manager
	sid     string

	mu          sync.Mutex
	constructed []Session
}

func newSessionFixture(t *testing.T) *sessionFixture {
	root, project := tempLayout(t)
	f := &sessionFixture{t: t, root: root, project: project, data: filepath.Join(root, "data")}
	f.store = openTestStore(t, f.data)
	f.player = newControlledPlayer()
	f.manager = NewManager(f.store, f.factory)
	p := must[Project](t)(f.store.AddProject(project))
	f.sid = must[Session](t)(f.store.CreateSession(p.ID, "codex", nil, "edit", nil, nil)).ID
	t.Cleanup(func() {
		f.manager.Close()
		f.store.Close()
	})
	return f
}

func (f *sessionFixture) factory(s Session, cwd string) (players.Player, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.constructed = append(f.constructed, s)
	return f.player, nil
}

func (f *sessionFixture) status() string {
	return must[Session](f.t)(f.store.Session(f.sid)).Status
}

func (f *sessionFixture) events() []Record {
	return must[[]Record](f.t)(f.store.Events(f.sid, 0, 1000))
}

func (f *sessionFixture) start(prompt string) {
	f.t.Helper()
	if err := f.manager.Start(f.sid, prompt); err != nil {
		f.t.Fatal(err)
	}
}

func isConflict(err error) bool {
	var c *ConflictError
	return errors.As(err, &c)
}

func next(t *testing.T, sub *Subscription) Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec, err := sub.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestSessionDisconnectReconnectReplaysAndTurnContinues(t *testing.T) {
	f := newSessionFixture(t)
	f.start("hello")
	sub := must[*Subscription](t)(f.manager.Subscribe(f.sid, 0))
	first := next(t, sub)
	if first.Type != "message.user" {
		t.Fatalf("first = %s", first.Payload)
	}
	sub.Close() // the UI disappears; the agent must keep running
	f.player.started.Wait(t)
	if f.player.Cancelled() {
		t.Fatal("subscriber disconnect cancelled the turn")
	}
	f.player.proceed.Set()
	f.manager.Wait(f.sid)
	replay := must[*Subscription](t)(f.manager.Subscribe(f.sid, first.Seq))
	defer replay.Close()
	var texts []string
	for {
		rec := next(t, replay)
		if rec.Type == players.TextDelta {
			texts = append(texts, rec.Fields()["text"].(string))
		}
		if rec.Type == players.TurnCompleted {
			break
		}
	}
	if !reflect.DeepEqual(texts, []string{"first", "last"}) {
		t.Fatalf("texts = %v", texts)
	}
	if s := must[Session](t)(f.store.Session(f.sid)); s.BackendSessionID == nil || *s.BackendSessionID != "backend-one" {
		t.Fatalf("backend id = %v", s.BackendSessionID)
	}
}

func TestSessionConcurrentTurnsAndSameProjectSessionsAreBlocked(t *testing.T) {
	f := newSessionFixture(t)
	f.start("first")
	if err := f.manager.Start(f.sid, "second"); !isConflict(err) {
		t.Fatalf("second turn err = %v", err)
	}
	s := must[Session](t)(f.store.Session(f.sid))
	other := must[Session](t)(f.store.CreateSession(s.ProjectID, "claude", nil, "edit", nil, nil))
	if err := f.manager.Start(other.ID, "also writes"); !isConflict(err) {
		t.Fatalf("same-project turn err = %v", err)
	}
	nested := filepath.Join(f.project, "sub")
	os.Mkdir(nested, 0o755)
	np := must[Project](t)(f.store.AddProject(nested))
	inner := must[Session](t)(f.store.CreateSession(np.ID, "claude", nil, "edit", nil, nil))
	if err := f.manager.Start(inner.ID, "nested folder"); !isConflict(err) {
		t.Fatalf("overlapping-folder turn err = %v", err)
	}
}

func TestSessionCancelImmediatelyReleasesWriterAndCanRestart(t *testing.T) {
	f := newSessionFixture(t)
	f.start("cancel immediately")
	if err := f.manager.Cancel(f.sid, false); err != nil {
		t.Fatal(err)
	}
	if st := f.status(); st != "cancelled" {
		t.Fatalf("status = %s", st)
	}
	lock := must[*bus.LockManager](t)(bus.NewLockManager(filepath.Join(f.project, ".orchestra")))
	if lock.IsLocked() {
		t.Fatal("writer lock still held after cancel")
	}
	f.player.proceed.Set()
	f.start("next")
	f.manager.Wait(f.sid)
	if st := f.status(); st != "idle" {
		t.Fatalf("status = %s", st)
	}
}

func TestSessionErrorWithoutTerminalEventMarksSessionError(t *testing.T) {
	f := newSessionFixture(t)
	f.player.send = func(ctx context.Context, prompt string, emit func(players.Event)) error {
		emit(players.NewEvent(players.Error, map[string]any{"message": "backend failed", "kind": "other", "retryable": false}))
		return nil
	}
	f.start("fail")
	f.manager.Wait(f.sid)
	if st := f.status(); st != "error" {
		t.Fatalf("status = %s", st)
	}
	ev := f.events()
	if ev[len(ev)-1].Fields()["stop_reason"] != "error" {
		t.Fatalf("last = %s", ev[len(ev)-1].Payload)
	}
}

func TestSessionSendErrorAndPanicBecomeTurnFailures(t *testing.T) {
	for name, send := range map[string]func(context.Context, string, func(players.Event)) error{
		"error": func(context.Context, string, func(players.Event)) error { return errors.New("boom") },
		"panic": func(context.Context, string, func(players.Event)) error { panic("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newSessionFixture(t)
			f.player.send = send
			f.start("fail")
			f.manager.Wait(f.sid)
			if st := f.status(); st != "error" {
				t.Fatalf("status = %s", st)
			}
			ev := f.events()
			msg, _ := ev[len(ev)-2].Fields()["message"].(string)
			if ev[len(ev)-1].Fields()["stop_reason"] != "error" || msg == "" {
				t.Fatalf("events = %v", ev)
			}
		})
	}
}

func TestSessionCompletedPlayerIsEvictedAndReconstructedForNextTurn(t *testing.T) {
	f := newSessionFixture(t)
	f.player.proceed.Set()
	f.start("first")
	f.manager.Wait(f.sid)
	f.manager.mu.Lock()
	_, cached := f.manager.players[f.sid]
	f.manager.mu.Unlock()
	if cached {
		t.Fatal("completed player still cached")
	}
	f.start("second")
	f.manager.Wait(f.sid)
	if len(f.constructed) != 2 {
		t.Fatalf("constructed %d players", len(f.constructed))
	}
	if h := f.player.History(); !reflect.DeepEqual(h, []string{"first", "second"}) {
		t.Fatalf("history = %v", h)
	}
}

func TestSessionUnchangedHistoryDoesNotSnapshotEachTextDelta(t *testing.T) {
	f := newSessionFixture(t)
	f.start("stream")
	f.player.started.Wait(t)
	for i := 0; i < 10; i++ {
		must[Record](t)(f.manager.Publish(f.sid, map[string]any{"type": "text.delta", "text": "x"}))
	}
	f.player.mu.Lock()
	calls := f.player.snapshotCalls
	f.player.mu.Unlock()
	if calls > 2 {
		t.Fatalf("snapshot taken %d times", calls)
	}
}

func TestSessionCleanupFailureBlocksProjectUntilExplicitConfirmation(t *testing.T) {
	f := newSessionFixture(t)
	f.player.cancelErr = errors.New("cleanup failed")
	f.player.proceed.Set()
	f.start("run")
	f.manager.Wait(f.sid)
	if st := f.status(); st != "cleanup_failed" {
		t.Fatalf("status = %s", st)
	}
	if err := f.manager.Start(f.sid, "unsafe retry"); !isConflict(err) {
		t.Fatalf("retry err = %v", err)
	}
	if err := f.manager.Cancel(f.sid, false); !isConflict(err) {
		t.Fatalf("cancel without confirmation err = %v", err)
	}
	if err := f.manager.Cancel(f.sid, true); err != nil {
		t.Fatal(err)
	}
	if st := f.status(); st != "cancelled" {
		t.Fatalf("status = %s", st)
	}
	lock := must[*bus.LockManager](t)(bus.NewLockManager(filepath.Join(f.project, ".orchestra")))
	if lock.IsLocked() {
		t.Fatal("writer lock still held after confirmation")
	}
}

func TestSessionImmediateCancelCleanupFailureIsDurableAndBlocked(t *testing.T) {
	f := newSessionFixture(t)
	f.player.cancelErr = errors.New("cleanup failed")
	f.start("cancel before start")
	if err := f.manager.Cancel(f.sid, false); err != nil {
		t.Fatal(err)
	}
	if st := f.status(); st != "cleanup_failed" {
		t.Fatalf("status = %s", st)
	}
	found := false
	for _, e := range f.events() {
		found = found || e.Type == players.TurnCompleted
	}
	if !found {
		t.Fatal("no turn.completed journaled")
	}
	if err := f.manager.Cancel(f.sid, true); err != nil {
		t.Fatal(err)
	}
}

func TestSessionMissingProjectIsValidationFailureBeforePlayerStarts(t *testing.T) {
	f := newSessionFixture(t)
	if err := os.Remove(f.project); err != nil {
		t.Fatal(err)
	}
	var re *RequestError
	if err := f.manager.Start(f.sid, "missing project"); !errors.As(err, &re) {
		t.Fatalf("err = %v", err)
	}
	if len(f.constructed) != 0 {
		t.Fatal("player constructed for a missing project")
	}
}

func TestSessionPromptValidation(t *testing.T) {
	f := newSessionFixture(t)
	for _, prompt := range []any{nil, 5, "", "  \n", string(make([]byte, maxPromptBytes+1))} {
		var re *RequestError
		if err := f.manager.Start(f.sid, prompt); !errors.As(err, &re) {
			t.Fatalf("Start(%T) err = %v", prompt, err)
		}
	}
	var nf *NotFoundError
	if err := f.manager.Start("missing", "hi"); !errors.As(err, &nf) {
		t.Fatalf("unknown session err = %v", err)
	}
}

type failedLock struct{}

func (failedLock) Heartbeat(string, int) (bool, error) { return false, errors.New("disk unavailable") }
func (failedLock) Release(string, bool) (bool, error)  { return false, errors.New("disk unavailable") }

func TestSessionHeartbeatExceptionCancelsTurnEvenIfReportingFails(t *testing.T) {
	f := newSessionFixture(t)
	f.manager.heartbeatEvery = time.Millisecond
	stopped := make(chan struct{})
	// An unknown session makes the diagnostic publish fail too.
	done := make(chan struct{})
	go func() {
		f.manager.heartbeat(context.Background(), "no-such-session", lease{lock: failedLock{}, owner: "owner"},
			func() { close(stopped) })
		close(done)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("lease failure did not stop the turn")
	}
	<-done
}

func TestSessionLostLeaseStopsRunningTurn(t *testing.T) {
	f := newSessionFixture(t)
	f.manager.heartbeatEvery = 20 * time.Millisecond
	f.start("long")
	f.player.started.Wait(t)
	lock := must[*bus.LockManager](t)(bus.NewLockManager(filepath.Join(f.project, ".orchestra")))
	if _, err := lock.Release("", true); err != nil { // someone force-releases the writer lock
		t.Fatal(err)
	}
	f.manager.Wait(f.sid)
	if st := f.status(); st != "cancelled" {
		t.Fatalf("status = %s", st)
	}
	found := false
	for _, e := range f.events() {
		if e.Type == players.Error && e.Fields()["message"] == "Project writer lease was lost; turn stopped." {
			found = true
		}
	}
	if !found {
		t.Fatal("lease loss not reported")
	}
}

func TestSessionPermissionAndCancelAreSessionScoped(t *testing.T) {
	f := newSessionFixture(t)
	f.player.permission = true
	f.start("ask")
	eventually(t, "pending permission", func() bool {
		f.manager.mu.Lock()
		defer f.manager.mu.Unlock()
		return f.manager.permissions[f.sid]["approval"]
	})
	if err := f.manager.Answer(f.sid, "unknown", true); !isConflict(err) {
		t.Fatalf("unknown request err = %v", err)
	}
	if err := f.manager.Answer(f.sid, "approval", false); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Answer(f.sid, "approval", false); !isConflict(err) {
		t.Fatalf("second answer err = %v", err)
	}
	f.manager.Wait(f.sid)
	if st := f.status(); st != "idle" {
		t.Fatalf("status after answered turn = %s", st)
	}
	denied := false
	for _, h := range f.player.History() {
		denied = denied || h == "denied"
	}
	if !denied {
		t.Fatalf("history = %v", f.player.History())
	}
	f.player.proceed.Clear()
	f.start("cancel")
	if err := f.manager.Cancel(f.sid, false); err != nil {
		t.Fatal(err)
	}
	if !f.player.Cancelled() {
		t.Fatal("player not cancelled")
	}
	ev := f.events()
	if ev[len(ev)-1].Fields()["stop_reason"] != "cancelled" {
		t.Fatalf("last = %s", ev[len(ev)-1].Payload)
	}
	if st := f.status(); st != "cancelled" {
		t.Fatalf("status = %s", st)
	}
}

func TestSessionRestartRestoresPlayerStateAndBackendID(t *testing.T) {
	f := newSessionFixture(t)
	f.player.proceed.Set()
	f.start("remember this")
	f.manager.Wait(f.sid)
	f.manager.Close()
	f.player = newControlledPlayer()
	f.player.proceed.Set()
	f.manager = NewManager(f.store, f.factory)
	f.start("follow up")
	f.manager.Wait(f.sid)
	if h := f.player.History(); !reflect.DeepEqual(h, []string{"remember this", "follow up"}) {
		t.Fatalf("history = %v", h)
	}
	last := f.constructed[len(f.constructed)-1]
	if last.BackendSessionID == nil || *last.BackendSessionID != "backend-one" {
		t.Fatalf("backend id = %v", last.BackendSessionID)
	}
}

func TestSessionInterruptedRestartRequiresStoppedProcessConfirmation(t *testing.T) {
	f := newSessionFixture(t)
	if err := f.store.UpdateSession(f.sid, map[string]any{"status": "running"}); err != nil {
		t.Fatal(err)
	}
	old := must[*bus.LockManager](t)(bus.NewLockManager(filepath.Join(f.project, ".orchestra")))
	if ok, err := old.Acquire(f.sid, "session-"+f.sid+"-old-launch", "local app turn", 120, false); !ok || err != nil {
		t.Fatalf("acquire %v %v", ok, err)
	}
	f.manager.Close()
	f.store.Close()
	f.store = openTestStore(t, f.data)
	f.manager = NewManager(f.store, f.factory)
	if err := f.manager.Start(f.sid, "resume without inspection"); !isConflict(err) {
		t.Fatalf("start err = %v", err)
	}
	if err := f.manager.Cancel(f.sid, true); err != nil {
		t.Fatal(err)
	}
	f.player.proceed.Set()
	f.start("continue after inspection")
	f.manager.Wait(f.sid)
	if st := f.status(); st != "idle" {
		t.Fatalf("status = %s", st)
	}
}

func TestSessionSlowSubscriberCannotStallTurnAndCanReplay(t *testing.T) {
	f := newSessionFixture(t)
	sub := must[*Subscription](t)(f.manager.Subscribe(f.sid, 0))
	defer sub.Close()
	pending := make(chan Record, 1)
	go func() {
		rec, _ := sub.Next(context.Background())
		pending <- rec
	}()
	time.Sleep(20 * time.Millisecond)
	f.start("start")
	select {
	case <-pending:
	case <-time.After(5 * time.Second):
		t.Fatal("no first record")
	}
	for i := 0; i < 200; i++ {
		must[Record](t)(f.manager.Publish(f.sid, map[string]any{"type": "text.delta", "text": "x"}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, err := sub.Next(ctx)
		if errors.Is(err, ErrSlowSubscriber) {
			break
		}
		if err != nil {
			t.Fatalf("err = %v", err)
		}
	}
	// The dropped subscriber can replay everything it missed.
	replay := must[*Subscription](t)(f.manager.Subscribe(f.sid, 0))
	defer replay.Close()
	count := 0
	for count < 200 {
		if next(t, replay).Type == players.TextDelta {
			count++
		}
	}
}

func TestSessionSubscribeRejectsOffsetsBeyondJournal(t *testing.T) {
	f := newSessionFixture(t)
	var re *RequestError
	if _, err := f.manager.Subscribe(f.sid, 1); !errors.As(err, &re) {
		t.Fatalf("after beyond last err = %v", err)
	}
	if _, err := f.manager.Subscribe(f.sid, -1); !errors.As(err, &re) {
		t.Fatalf("negative after err = %v", err)
	}
	var nf *NotFoundError
	if _, err := f.manager.Subscribe("missing", 0); !errors.As(err, &nf) {
		t.Fatalf("unknown session err = %v", err)
	}
}

func TestSessionCloseCancelsRunningTurnsAndRefusesNewOnes(t *testing.T) {
	f := newSessionFixture(t)
	f.start("long")
	f.player.started.Wait(t)
	f.manager.Close()
	if !f.player.Cancelled() {
		t.Fatal("running turn not cancelled at shutdown")
	}
	if st := f.status(); st != "cancelled" {
		t.Fatalf("status = %s", st)
	}
	if err := f.manager.Start(f.sid, "after close"); !isConflict(err) {
		t.Fatalf("start after close err = %v", err)
	}
}

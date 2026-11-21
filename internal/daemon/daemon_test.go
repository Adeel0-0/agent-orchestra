package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Adeel0-0/agent-orchestra/internal/config"
	"github.com/Adeel0-0/agent-orchestra/internal/model"
	"github.com/Adeel0-0/agent-orchestra/internal/policy"
)

// Ported from tests/test_daemon.py. The verify commands are the test binary
// itself (helper-process pattern), run through the shell like real pipelines.
const helperEnv = "ORCHESTRA_DAEMON_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "verify-lock-held":
		// Passes only while the daemon itself holds the writer lock.
		var lock map[string]any
		data, err := os.ReadFile(filepath.Join(".orchestra", "writer.lock"))
		if err != nil || json.Unmarshal(data, &lock) != nil || lock["agent_id"] != "orchestra" {
			os.Exit(1)
		}
		os.Exit(0)
	case "verify-fails":
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type repoFixture struct {
	t       *testing.T
	repo    string
	baseSHA string
}

// newRepo is a throwaway repo on main with one commit; .orchestra/ is ignored.
func newRepo(t *testing.T) *repoFixture {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "test")
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".orchestra/\n"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")
	return &repoFixture{t: t, repo: repo, baseSHA: git(t, repo, "rev-parse", "HEAD")}
}

func (f *repoFixture) head() string { return git(f.t, f.repo, "rev-parse", "HEAD") }

func (f *repoFixture) daemon(verifyHelper string) *Daemon {
	f.t.Helper()
	f.t.Setenv(helperEnv, verifyHelper)
	cfg := config.Default()
	cfg.WorkspaceRoot = f.repo
	cfg.Git.AutoPush = false
	verify := `"` + os.Args[0] + `" -test.run=NONE`
	cfg.Pipeline.VerifyCommand = &verify
	d, err := New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

// agentFinishes does what an agent does: edit, claim, release with a status.
func (f *repoFixture) agentFinishes(d *Daemon, status string) {
	f.t.Helper()
	os.WriteFile(filepath.Join(f.repo, "feature.py"), []byte("def feature(): ...\n"), 0o644)
	if ok, err := Claim(d.Locks, d.Bus, "TASK-1", "claude", "", 1800, false); !ok || err != nil {
		f.t.Fatalf("claim: %v %v", ok, err)
	}
	if res, err := Release(d.Locks, d.Bus, "claude", status, "", false); !res.Released || err != nil {
		f.t.Fatalf("release: %+v %v", res, err)
	}
}

func eventsOf(d *Daemon, t model.EventType) []model.AgentEvent {
	var out []model.AgentEvent
	for _, e := range d.Bus.ReadEvents(0, 1000) {
		if e.EventType == t {
			out = append(out, e)
		}
	}
	return out
}

func tick(t *testing.T, d *Daemon) {
	t.Helper()
	if _, err := d.Tick(); err != nil {
		t.Fatal(err)
	}
}

func TestDirtyTreeWithoutHandoffRunsNothing(t *testing.T) {
	f := newRepo(t)
	d := f.daemon("verify-lock-held")
	os.WriteFile(filepath.Join(f.repo, "wip.py"), []byte("x = 1\n"), 0o644)
	tick(t, d)
	tick(t, d)
	if len(eventsOf(d, model.VerificationStarted)) != 0 || f.head() != f.baseSHA || d.State != model.ReadyNeedsVerify {
		t.Fatalf("state %s, head moved %v", d.State, f.head() != f.baseSHA)
	}
}

func TestPausedHandoffIsNeverCommitted(t *testing.T) {
	f := newRepo(t)
	d := f.daemon("verify-lock-held")
	f.agentFinishes(d, "paused")
	tick(t, d)
	if len(eventsOf(d, model.VerificationStarted)) != 0 || f.head() != f.baseSHA || d.State != model.PausedCheckpoint {
		t.Fatalf("state %s", d.State)
	}
	found := false
	for _, e := range eventsOf(d, model.CustomEvent) {
		p := e.Payload
		if p["action"] == "handoff_skipped" && p["reason"] == "policy" && p["task_id"] == "TASK-1" && p["status"] == "paused" {
			found = true
		}
	}
	if !found {
		t.Fatal("handoff_skipped event missing")
	}
}

func TestShippableHandoffRunsPipelineOnceUnderDaemonLock(t *testing.T) {
	f := newRepo(t)
	d := f.daemon("verify-lock-held")
	var calls []model.HandoffPayload
	var lastLock map[string]any
	d.ShouldShip = func(h model.HandoffPayload, lock map[string]any) bool {
		calls = append(calls, h)
		lastLock = lock
		return policy.ShouldShip(h, lock)
	}
	f.agentFinishes(d, "complete")
	tick(t, d)
	tick(t, d)

	if len(calls) != 1 || calls[0].TaskID != "TASK-1" || calls[0].AgentID != "claude" || calls[0].Status != "complete" {
		t.Fatalf("policy calls %+v", calls)
	}
	if lastLock["agent_id"] != "claude" {
		t.Fatalf("released_lock %v", lastLock)
	}
	if n := len(eventsOf(d, model.VerificationStarted)); n != 1 {
		t.Fatalf("verification ran %d times", n)
	}
	results := eventsOf(d, model.VerificationResult)
	if len(results) != 1 || results[0].Payload["success"] != true {
		t.Fatalf("verify did not see the orchestra lock: %+v", results)
	}
	if f.head() == f.baseSHA || !strings.Contains(git(t, f.repo, "show", "--name-only", "--format=", "HEAD"), "feature.py") {
		t.Fatal("handoff was not committed")
	}
	if d.Locks.IsLocked() {
		t.Fatal("daemon kept the writer lock")
	}
}

func TestFailedVerificationIsNotRetriedEveryTick(t *testing.T) {
	f := newRepo(t)
	d := f.daemon("verify-fails")
	f.agentFinishes(d, "complete")
	for i := 0; i < 3; i++ {
		tick(t, d)
	}
	if n := len(eventsOf(d, model.VerificationStarted)); n != 1 {
		t.Fatalf("verification ran %d times", n)
	}
	if d.State != model.Error || f.head() != f.baseSHA {
		t.Fatalf("state %s", d.State)
	}
}

func TestOnErrorCommandRunsOnce(t *testing.T) {
	f := newRepo(t)
	d := f.daemon("verify-fails")
	marker := filepath.Join(t.TempDir(), "errors.txt")
	onError := `echo x>>"` + marker + `"`
	d.Config.Pipeline.OnErrorCommand = &onError
	f.agentFinishes(d, "complete")
	for i := 0; i < 3; i++ {
		tick(t, d)
	}
	data, _ := os.ReadFile(marker)
	if got := strings.Count(string(data), "x"); got != 1 {
		t.Fatalf("on_error_command ran %d times", got)
	}
}

func TestHandoffSkippedWhenAnotherWriterClaimedFirst(t *testing.T) {
	f := newRepo(t)
	d := f.daemon("verify-lock-held")
	f.agentFinishes(d, "complete")
	Claim(d.Locks, d.Bus, "TASK-2", "codex", "", 1800, false)
	tick(t, d)
	if len(eventsOf(d, model.VerificationStarted)) != 0 || f.head() != f.baseSHA || d.Locks.CurrentLock()["agent_id"] != "codex" {
		t.Fatal("pipeline ran over another writer")
	}
	skipped := false
	for _, e := range eventsOf(d, model.CustomEvent) {
		if e.Payload["reason"] == "writer_active" {
			skipped = true
		}
	}
	if !skipped {
		t.Fatal("writer_active skip not emitted")
	}
}

func TestFirstStartDoesNotReplayOldHandoffs(t *testing.T) {
	f := newRepo(t)
	setup := f.daemon("verify-lock-held")
	f.agentFinishes(setup, "complete")
	d := f.daemon("verify-lock-held") // no cursor file yet: starts at the end
	tick(t, d)
	if len(eventsOf(d, model.VerificationStarted)) != 0 {
		t.Fatal("old handoff replayed on first start")
	}
}

func TestHandoffSentWhileDaemonDownRunsOnRestart(t *testing.T) {
	f := newRepo(t)
	if err := f.daemon("verify-lock-held").SaveCursor(); err != nil { // what Start records before its loop
		t.Fatal(err)
	}
	f.agentFinishes(f.daemon("verify-lock-held"), "complete")
	restarted := f.daemon("verify-lock-held")
	tick(t, restarted)
	if n := len(eventsOf(restarted, model.VerificationStarted)); n != 1 {
		t.Fatalf("verification ran %d times after restart", n)
	}
}

func TestReleasePayloadCarriesTheReleasedLock(t *testing.T) {
	f := newRepo(t)
	d := f.daemon("verify-lock-held")
	f.agentFinishes(d, "complete")
	handoffs := eventsOf(d, model.HandoffRequest)
	lock, ok := handoffs[0].Payload["released_lock"].(map[string]any)
	if len(handoffs) != 1 || !ok || lock["agent_id"] != "claude" || handoffs[0].Payload["task_id"] != "TASK-1" {
		t.Fatalf("%+v", handoffs)
	}
	// Releasing with no lock held records null.
	Release(d.Locks, d.Bus, "claude", "complete", "", false)
	last := eventsOf(d, model.HandoffRequest)[1]
	if v, present := last.Payload["released_lock"]; !present || v != nil {
		t.Fatalf("released_lock should be null: %+v", last.Payload)
	}
}

func TestCoordinationFileReleaseIsNeverShipped(t *testing.T) {
	f := newRepo(t)
	d := f.daemon("verify-lock-held")
	coord := "coordination.md"
	d.Config.CoordinationFile = &coord
	os.WriteFile(filepath.Join(f.repo, coord), []byte("- **Active writer:** claude, auth work\r\n"), 0o644)
	d.CheckCoordinationFile()
	if d.Locks.CurrentLock()["agent_id"] != "claude" {
		t.Fatalf("coordination claim not mirrored: %v", d.Locks.CurrentLock())
	}
	os.WriteFile(filepath.Join(f.repo, coord), []byte("- **Active writer:** none\n"), 0o644)
	d.CheckCoordinationFile()
	tick(t, d)
	handoffs := eventsOf(d, model.HandoffRequest)
	if len(handoffs) != 1 || handoffs[0].Payload["status"] != "unknown" || handoffs[0].Payload["released_lock"] == nil {
		t.Fatalf("%+v", handoffs)
	}
	if len(eventsOf(d, model.VerificationStarted)) != 0 {
		t.Fatal("a coordination-file release shipped")
	}
}

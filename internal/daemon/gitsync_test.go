package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Adeel0-0/agent-orchestra/internal/model"
)

// Pipeline runs that push to a real bare origin (TestGitSyncAgainstRemote in Python).
type remoteFixture struct {
	*repoFixture
	origin string
}

func newRemoteRepo(t *testing.T) *remoteFixture {
	f := newRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, f.repo, "init", "-q", "--bare", "-b", "main", origin)
	git(t, f.repo, "remote", "add", "origin", origin)
	git(t, f.repo, "push", "-q", "origin", "main", "main:go-backend")
	return &remoteFixture{repoFixture: f, origin: origin}
}

func (f *remoteFixture) pushingDaemon() *Daemon {
	d := f.daemon("verify-lock-held")
	d.Config.Git.AutoPush = true
	d.Config.Git.SyncBranches = []string{"go-backend"}
	return d
}

func (f *remoteFixture) originSHA(branch string) string {
	return git(f.t, f.origin, "rev-parse", branch)
}

func syncFailures(d *Daemon) []string {
	steps := []string{}
	for _, e := range eventsOf(d, model.GitSyncFailed) {
		steps = append(steps, e.Payload["step"].(string))
	}
	return steps
}

func TestSuccessfulHandoffPushesMainAndFastForwardsSyncBranch(t *testing.T) {
	f := newRemoteRepo(t)
	d := f.pushingDaemon()
	f.agentFinishes(d, "complete")
	tick(t, d)

	head := f.head()
	if head == f.baseSHA || f.originSHA("main") != head || f.originSHA("go-backend") != head {
		t.Fatalf("head %s origin main %s go-backend %s", head, f.originSHA("main"), f.originSHA("go-backend"))
	}
	if b := git(t, f.repo, "branch", "--show-current"); b != "main" {
		t.Fatalf("a checkout happened: on %s", b)
	}
	if fails := syncFailures(d); len(fails) != 0 {
		t.Fatalf("sync failures %v", fails)
	}
	synced := eventsOf(d, model.GitSynced)
	if len(synced) != 1 || synced[0].Payload["pushed"] != true || len(synced[0].Payload["synced_branches"].([]any)) != 1 {
		t.Fatalf("GIT_SYNCED %+v", synced)
	}
	if d.State != model.IdleClean {
		t.Fatalf("state %s", d.State)
	}
}

func TestRebaseConflictIsAbortedAndNothingIsPushed(t *testing.T) {
	f := newRemoteRepo(t)
	// Someone else pushes a conflicting feature.py to origin first.
	other := filepath.Join(t.TempDir(), "other")
	git(t, f.repo, "clone", "-q", f.origin, other)
	git(t, other, "config", "user.email", "other@example.com")
	git(t, other, "config", "user.name", "other")
	os.WriteFile(filepath.Join(other, "feature.py"), []byte("def feature(): return 'theirs'\n"), 0o644)
	git(t, other, "add", ".")
	git(t, other, "commit", "-q", "-m", "theirs")
	git(t, other, "push", "-q", "origin", "main")
	theirs := f.originSHA("main")

	d := f.pushingDaemon()
	f.agentFinishes(d, "complete")
	tick(t, d)

	if fails := syncFailures(d); len(fails) != 1 || fails[0] != "rebase" {
		t.Fatalf("sync failures %v", fails)
	}
	if d.State != model.Error {
		t.Fatalf("state %s", d.State)
	}
	if f.originSHA("main") != theirs || f.originSHA("go-backend") != f.baseSHA {
		t.Fatal("origin changed after a failed rebase")
	}
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(f.repo, ".git", dir)); err == nil {
			t.Fatalf(".git/%s left behind: rebase was not aborted", dir)
		}
	}
	if s := git(t, f.repo, "status", "--porcelain"); s != "" {
		t.Fatalf("tree not clean after abort: %q", s)
	}
	errText := eventsOf(d, model.GitSyncFailed)[0].Payload["error"].(string)
	if errText == "" || len(errText) > 2000*4 {
		t.Fatalf("error text %q", errText)
	}
}

func TestWrongBranchCheckedOutCommitsNothing(t *testing.T) {
	f := newRemoteRepo(t)
	git(t, f.repo, "switch", "-q", "-c", "experiment")
	d := f.pushingDaemon()
	f.agentFinishes(d, "complete")
	tick(t, d)

	if fails := syncFailures(d); len(fails) != 1 || fails[0] != "branch" {
		t.Fatalf("sync failures %v", fails)
	}
	if f.head() != f.baseSHA || f.originSHA("main") != f.baseSHA || d.State != model.Error {
		t.Fatalf("committed or pushed on the wrong branch; state %s", d.State)
	}
}

func TestPostSyncCommandIsSkippedAfterASyncFailure(t *testing.T) {
	f := newRemoteRepo(t)
	git(t, f.repo, "switch", "-q", "-c", "experiment")
	d := f.pushingDaemon()
	marker := filepath.Join(t.TempDir(), "post.txt")
	post := `echo ran>"` + marker + `"`
	d.Config.Pipeline.PostSyncCommand = &post
	f.agentFinishes(d, "complete")
	tick(t, d)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("post_sync_command ran after a failed sync")
	}
}

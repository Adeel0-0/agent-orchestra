// Package daemon is the conductor (orchestra/daemon.py): it reads handoff events
// from a persisted cursor and runs verify, commit and push once per handoff that
// the policy approves. A dirty tree on its own never triggers anything.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Adeel0-0/agent-orchestra/internal/bus"
	"github.com/Adeel0-0/agent-orchestra/internal/config"
	"github.com/Adeel0-0/agent-orchestra/internal/gitsync"
	"github.com/Adeel0-0/agent-orchestra/internal/model"
	"github.com/Adeel0-0/agent-orchestra/internal/policy"
	"github.com/Adeel0-0/agent-orchestra/internal/runner"
	"github.com/Adeel0-0/agent-orchestra/internal/watcher"
)

const orchestraAgent = "orchestra"

// Daemon holds one workspace's state machine.
type Daemon struct {
	Config config.Config
	Root   string
	Dir    string
	Bus    *bus.EventBus
	Locks  *bus.LockManager
	Runner *runner.Runner
	Git    *gitsync.Engine
	State  model.State
	// ShouldShip is the shipping policy; tests may wrap it to observe its inputs.
	ShouldShip func(model.HandoffPayload, map[string]any) bool

	cursorFile string
	cursor     int64
}

// New opens the workspace from cfg and loads the event cursor.
func New(cfg config.Config) (*Daemon, error) {
	root, err := filepath.Abs(cfg.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, cfg.OrchestraDir)
	b, err := bus.NewEventBus(dir)
	if err != nil {
		return nil, err
	}
	locks, err := bus.NewLockManager(dir)
	if err != nil {
		return nil, err
	}
	d := &Daemon{
		Config: cfg, Root: root, Dir: dir, Bus: b, Locks: locks,
		Runner: runner.New(root), Git: gitsync.New(root),
		State: model.IdleClean, ShouldShip: policy.ShouldShip,
		cursorFile: filepath.Join(dir, "daemon_cursor.json"),
	}
	d.cursor = d.loadCursor()
	return d, nil
}

// loadCursor resumes where the last daemon stopped. A first run starts at the
// end of the log, so old handoffs are never replayed.
func (d *Daemon) loadCursor() int64 {
	data, err := os.ReadFile(d.cursorFile)
	if err == nil {
		var c struct {
			Offset *float64 `json:"offset"`
		}
		if json.Unmarshal(data, &c) == nil && c.Offset != nil {
			return int64(*c.Offset)
		}
	}
	return d.Bus.EndOffset()
}

// SaveCursor writes daemon_cursor.json through a temp file.
func (d *Daemon) SaveCursor() error {
	tmp := strings.TrimSuffix(d.cursorFile, ".json") + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf(`{"offset": %d}`, d.cursor)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, d.cursorFile)
}

// GitInfo is the "git" block of the status report.
type GitInfo struct {
	Branch       string `json:"branch"`
	HeadSHA      string `json:"head_sha"`
	IsClean      bool   `json:"is_clean"`
	ChangedCount int    `json:"changed_count"`
}

// Status is the status report, with Python's key order.
type Status struct {
	State       model.State       `json:"state"`
	IsLocked    bool              `json:"is_locked"`
	ActiveLock  map[string]any    `json:"active_lock"`
	Git         *GitInfo          `json:"git"`
	LatestEvent *model.AgentEvent `json:"latest_event"`
}

// GetStatus reports the current state.
func (d *Daemon) GetStatus() Status {
	st := Status{State: d.State, IsLocked: d.Locks.IsLocked(), ActiveLock: d.Locks.CurrentLock(), LatestEvent: d.Bus.Latest()}
	if d.Git.IsGitRepo() {
		g := d.Git.Status()
		st.Git = &GitInfo{Branch: g.Branch, HeadSHA: g.HeadSHA, IsClean: g.IsClean, ChangedCount: len(g.ChangedFiles)}
	}
	return st
}

// Tick consumes new events (a handoff may run the pipeline), then refreshes the state.
func (d *Daemon) Tick() (Status, error) {
	events, next, err := d.Bus.ReadFrom(d.cursor)
	if err != nil {
		return Status{}, err
	}
	if next != d.cursor {
		// Advance before handling, so a crash mid-pipeline never re-ships the same handoff.
		d.cursor = next
		if err := d.SaveCursor(); err != nil {
			return Status{}, err
		}
	}
	for _, evt := range events {
		if err := d.handleEvent(evt); err != nil {
			return Status{}, err
		}
	}

	switch {
	case d.Locks.IsLocked():
		d.State = model.WriterActive
	case d.State == model.Error || d.State == model.PausedCheckpoint:
		// Held until the next writer claims.
	default:
		// A dirty tree is only reported. Nothing ships without a handoff event.
		dirty := false
		if d.Git.IsGitRepo() {
			dirty = !d.Git.Status().IsClean
		}
		if dirty {
			d.State = model.ReadyNeedsVerify
		} else {
			d.State = model.IdleClean
		}
	}
	return d.GetStatus(), nil
}

func (d *Daemon) handleEvent(evt model.AgentEvent) error {
	if evt.EventType != model.HandoffRequest {
		return nil
	}
	data := make(map[string]any, len(evt.Payload)+2)
	for k, v := range evt.Payload {
		data[k] = v
	}
	data["agent_id"] = evt.SourceAgent
	data["timestamp"] = evt.Timestamp
	handoff := model.HandoffFromDict(data)
	released, _ := evt.Payload["released_lock"].(map[string]any)

	if !d.ShouldShip(handoff, released) {
		if handoff.Status == "paused" {
			d.State = model.PausedCheckpoint
		}
		return d.emitSkipped(handoff, "policy")
	}

	// Hold the writer lock so no agent starts editing while we verify and commit.
	ok, err := d.Locks.Acquire(handoff.TaskID, orchestraAgent, "handoff pipeline", d.Config.LockLeaseSeconds, false)
	if err != nil {
		return err
	}
	if !ok {
		return d.emitSkipped(handoff, "writer_active")
	}
	defer d.Locks.Release(orchestraAgent, false)
	d.RunPipeline()
	return nil
}

func (d *Daemon) emitSkipped(h model.HandoffPayload, reason string) error {
	_, err := d.Bus.Emit(string(model.CustomEvent), orchestraAgent, map[string]any{
		"action": "handoff_skipped", "reason": reason, "task_id": h.TaskID, "status": h.Status,
	})
	return err
}

func (d *Daemon) emit(t model.EventType, payload map[string]any) {
	d.Bus.Emit(string(t), orchestraAgent, payload)
}

// RunPipeline verifies, commits, syncs and runs the post-sync hook. A failure
// sets ERROR and runs on_error_command once; nothing re-runs on later ticks.
func (d *Daemon) RunPipeline() bool {
	verify := config.Str(d.Config.Pipeline.VerifyCommand)
	if verify == "" {
		return true
	}
	d.State = model.Verifying
	d.emit(model.VerificationStarted, map[string]any{"command": verify})
	res := d.Runner.Run(runner.ShellCommand(verify), runner.Options{})
	d.emit(model.VerificationResult, res.Payload())
	if !res.Success {
		d.failPipeline()
		return false
	}

	d.State = model.Syncing
	if d.Config.Git.Enabled && d.Git.IsGitRepo() {
		if !d.executeGitSync() {
			d.failPipeline()
			return false
		}
	}
	if post := config.Str(d.Config.Pipeline.PostSyncCommand); post != "" {
		d.Runner.Run(runner.ShellCommand(post), runner.Options{})
	}
	d.State = model.IdleClean
	return true
}

func (d *Daemon) failPipeline() {
	d.State = model.Error
	if cmd := config.Str(d.Config.Pipeline.OnErrorCommand); cmd != "" {
		d.Runner.Run(runner.ShellCommand(cmd), runner.Options{})
	}
}

func (d *Daemon) syncFailed(step, errText string) bool {
	d.emit(model.GitSyncFailed, map[string]any{"step": step, "error": errText})
	return false
}

// executeGitSync stops at the first failing step and emits GIT_SYNC_FAILED naming it.
func (d *Daemon) executeGitSync() bool {
	cfg := d.Config.Git
	st := d.Git.Status()
	if st.Branch != cfg.MainBranch {
		return d.syncFailed("branch", fmt.Sprintf("checked out '%s', expected '%s'", st.Branch, cfg.MainBranch))
	}

	if !st.IsClean && cfg.AutoCommit {
		msg := cfg.CommitPrefix + " automated multi-agent handoff sync\n\nVerified by Agent Orchestra pipeline."
		if !d.Git.StageAll() {
			return d.syncFailed("stage", d.Git.LastError)
		}
		if !d.Git.Commit(msg) {
			return d.syncFailed("commit", d.Git.LastError)
		}
		d.emit(model.GitCommitted, map[string]any{"commit_message": msg})
	}

	// Everything below publishes the commit, so it only runs when pushing.
	if cfg.AutoPush {
		if !d.Git.Fetch(cfg.Remote) {
			return d.syncFailed("fetch", d.Git.LastError)
		}
		if !d.Git.Rebase(cfg.Remote + "/" + cfg.MainBranch) {
			return d.syncFailed("rebase", d.Git.LastError)
		}
		if !d.Git.Push(cfg.Remote, cfg.MainBranch) {
			return d.syncFailed("push", d.Git.LastError)
		}
		for _, branch := range cfg.SyncBranches {
			if !d.Git.FastForwardBranch(branch, cfg.MainBranch, cfg.Remote) {
				return d.syncFailed("sync:"+branch, d.Git.LastError)
			}
		}
	}

	synced := []string{}
	if cfg.AutoPush {
		synced = append(synced, cfg.SyncBranches...)
	}
	d.emit(model.GitSynced, map[string]any{"main_branch": cfg.MainBranch, "synced_branches": synced, "pushed": cfg.AutoPush})
	return true
}

// OnFilesystemChange emits TASK_PROGRESS and checks the coordination file.
func (d *Daemon) OnFilesystemChange(paths []string) {
	rel := []string{}
	for _, p := range paths {
		r, err := filepath.Rel(d.Root, p)
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			continue
		}
		rel = append(rel, r)
	}
	lock := d.Locks.CurrentLock()
	source := "untracked"
	var taskID any
	if lock != nil {
		source = model.PyStr(lock, "agent_id", "unknown")
		taskID = lock["task_id"]
	}
	sample := rel
	if len(sample) > 5 {
		sample = sample[:5]
	}
	d.Bus.Emit(string(model.TaskProgress), source, map[string]any{
		"changed_count": len(rel), "sample_files": sample, "task_id": taskID,
	})

	coord := config.Str(d.Config.CoordinationFile)
	if coord == "" {
		return
	}
	want := strings.ReplaceAll(coord, `\`, "/")
	for _, r := range rel {
		if strings.ReplaceAll(r, `\`, "/") == want {
			d.CheckCoordinationFile()
			return
		}
	}
}

// Same pattern as Python (re.MULTILINE); the captured text is trimmed, which drops a trailing \r.
var activeWriterLine = regexp.MustCompile(`(?m)-\s+\*\*Active writer:\*\*\s*(.+)$`)

// CheckCoordinationFile mirrors the writer named in a coordination file
// (for example docs/coordination.md) into the lock.
func (d *Daemon) CheckCoordinationFile() {
	coord := config.Str(d.Config.CoordinationFile)
	if coord == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(d.Root, coord))
	if err != nil {
		return
	}
	m := activeWriterLine.FindStringSubmatch(string(data))
	if m == nil {
		return
	}
	writer := strings.TrimSpace(m[1])
	if strings.HasPrefix(strings.ToLower(writer), "none") {
		if d.Locks.IsLocked() {
			current := d.Locks.CurrentLock()
			d.Locks.Release("", true)
			source := "agent"
			if current != nil {
				source = model.PyStr(current, "agent_id", "agent")
			}
			d.Bus.Emit(string(model.HandoffRequest), source, map[string]any{
				"note": "Writer released via coordination file",
				// The file says the writer left, not that the work is finished.
				"status":        "unknown",
				"released_lock": lockOrNil(current),
			})
		}
		return
	}
	agent := strings.TrimSpace(strings.SplitN(writer, ",", 2)[0])
	if !d.Locks.IsLocked() {
		d.Locks.Acquire("auto_task", agent, writer, 1800, false)
		d.Bus.Emit(string(model.AgentClaim), agent, map[string]any{"scope": writer})
	}
}

func lockOrNil(lock map[string]any) any {
	if lock == nil {
		return nil
	}
	return lock
}

// Start saves the cursor (so handoffs sent while the daemon is down run on
// restart), starts the watcher, and ticks every interval until ctx is done.
func (d *Daemon) Start(ctx context.Context, interval time.Duration) error {
	if err := d.SaveCursor(); err != nil {
		return err
	}
	w := watcher.New(d.Root, d.Config.WatchPaths, d.Config.IgnorePatterns,
		time.Duration(d.Config.DebounceMS)*time.Millisecond, d.OnFilesystemChange)
	if err := w.Start(); err != nil {
		return err
	}
	defer w.Stop()
	for {
		if _, err := d.Tick(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// Claim acquires the writer lock for an agent and emits AGENT_CLAIM (what `orchestra claim` does).
func Claim(locks *bus.LockManager, b *bus.EventBus, taskID, agent, scope string, lease int, force bool) (bool, error) {
	ok, err := locks.Acquire(taskID, agent, scope, lease, force)
	if err != nil || !ok {
		return ok, err
	}
	_, err = b.Emit(string(model.AgentClaim), agent, map[string]any{"task_id": taskID, "scope": scope})
	return true, err
}

// ReleaseResult describes what `orchestra release` did.
type ReleaseResult struct {
	Released     bool
	HoldingAgent string
	TaskID       string
}

// Release releases the writer lock and emits HANDOFF_REQUEST carrying the lock
// as it was just before release ("released_lock", or null).
func Release(locks *bus.LockManager, b *bus.EventBus, agent, status, summary string, force bool) (ReleaseResult, error) {
	current := locks.CurrentLock()
	res := ReleaseResult{HoldingAgent: agent, TaskID: "unknown"}
	if current != nil {
		res.HoldingAgent = model.PyStr(current, "agent_id", "")
		res.TaskID = model.PyStr(current, "task_id", "")
	}
	ok, err := locks.Release(agent, force)
	if err != nil || !ok {
		return res, err
	}
	res.Released = true
	source := agent
	if source == "" {
		source = "unknown"
	}
	_, err = b.Emit(string(model.HandoffRequest), source, map[string]any{
		"task_id": res.TaskID, "status": status, "summary": summary, "released_lock": lockOrNil(current),
	})
	return res, err
}

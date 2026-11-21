package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Adeel0-0/agent-orchestra/internal/model"
	"github.com/Adeel0-0/agent-orchestra/internal/pyjson"
)

const (
	sharingRetries       = 5
	sharingRetryInterval = 50 * time.Millisecond
)

// LockManager is the lease-based single-writer lock in <orchestra_dir>/writer.lock.
// Every read-decide-write happens while holding an OS lock on writer.lock.guard,
// so two agents (in Go or Python) can never both see "unlocked" and both claim it.
type LockManager struct {
	Dir       string
	LockFile  string
	GuardFile string
}

// NewLockManager creates the directory if needed.
func NewLockManager(dir string) (*LockManager, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &LockManager{
		Dir:       abs,
		LockFile:  filepath.Join(abs, "writer.lock"),
		GuardFile: filepath.Join(abs, "writer.lock.guard"),
	}, nil
}

// lockRecord keeps Python's key order for newly written locks.
type lockRecord struct {
	TaskID    string  `json:"task_id"`
	AgentID   string  `json:"agent_id"`
	Scope     string  `json:"scope"`
	ClaimedAt float64 `json:"claimed_at"`
	ExpiresAt float64 `json:"expires_at"`
	PID       int     `json:"pid"`
}

// withGuard runs fn while holding the guard lock and always unlocks before closing.
func (m *LockManager) withGuard(fn func() (bool, error)) (bool, error) {
	f, err := os.OpenFile(m.GuardFile, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	unlock, err := lockGuardFile(f)
	if err != nil {
		return false, err
	}
	ok, fnErr := fn()
	if err := unlock(); err != nil && fnErr == nil {
		fnErr = fmt.Errorf("unlocking %s: %w", m.GuardFile, err)
	}
	return ok, fnErr
}

// Acquire claims the writer lock. It refuses (false, nil) when an unexpired lock is
// held by a different agent, unless force. The same agent claiming again refreshes the lease.
func (m *LockManager) Acquire(taskID, agentID, scope string, leaseSeconds int, force bool) (bool, error) {
	return m.withGuard(func() (bool, error) {
		now := model.Now()
		if current := m.CurrentLock(); current != nil && !force {
			if now < model.LockExpiresAt(current) && current["agent_id"] != any(agentID) {
				return false, nil
			}
		}
		return m.write(lockRecord{
			TaskID: taskID, AgentID: agentID, Scope: scope,
			ClaimedAt: now, ExpiresAt: now + float64(leaseSeconds), PID: os.Getpid(),
		})
	})
}

// Release removes the lock. With a non-empty agentID that is not the holder it
// refuses (false, nil) unless force. Releasing when nothing is held succeeds.
func (m *LockManager) Release(agentID string, force bool) (bool, error) {
	return m.withGuard(func() (bool, error) {
		if _, err := os.Stat(m.LockFile); errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		if current := m.CurrentLock(); current != nil && !force && agentID != "" {
			if current["agent_id"] != any(agentID) {
				return false, nil
			}
		}
		err := retrySharing(func() error {
			err := os.Remove(m.LockFile)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		})
		return err == nil, nil
	})
}

// Heartbeat extends the lease if agentID holds the lock.
func (m *LockManager) Heartbeat(agentID string, extendSeconds int) (bool, error) {
	return m.withGuard(func() (bool, error) {
		current := m.CurrentLock()
		if current == nil || current["agent_id"] != any(agentID) {
			return false, nil
		}
		current["expires_at"] = model.Now() + float64(extendSeconds)
		return m.write(current)
	})
}

// CurrentLock reads writer.lock without the guard. Missing, unreadable, corrupt
// or non-object content all mean "no lock".
func (m *LockManager) CurrentLock() map[string]any {
	data, err := os.ReadFile(m.LockFile)
	if err != nil {
		return nil
	}
	var lock map[string]any
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil
	}
	return lock
}

// IsLocked reports whether an unexpired lock is held.
func (m *LockManager) IsLocked() bool {
	lock := m.CurrentLock()
	return lock != nil && model.Now() < model.LockExpiresAt(lock)
}

// write replaces writer.lock through a temp file, so readers never see a partial lock.
func (m *LockManager) write(v any) (bool, error) {
	data, err := pyjson.MarshalIndent(v)
	if err != nil {
		return false, err
	}
	tmp := filepath.Join(m.Dir, ".lock_"+model.RandomHex(3)+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return false, nil
	}
	if err := retrySharing(func() error { return os.Rename(tmp, m.LockFile) }); err != nil {
		os.Remove(tmp)
		return false, nil
	}
	return true, nil
}

// retrySharing retries an operation that Windows can briefly refuse while another
// process has the file open. Other errors, and every error elsewhere, return at once.
func retrySharing(op func() error) error {
	var err error
	for attempt := 0; attempt < sharingRetries; attempt++ {
		if err = op(); err == nil || !isSharingViolation(err) {
			return err
		}
		time.Sleep(sharingRetryInterval)
	}
	return err
}

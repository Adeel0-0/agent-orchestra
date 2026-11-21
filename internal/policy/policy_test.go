package policy

import (
	"testing"

	"github.com/Adeel0-0/agent-orchestra/internal/model"
)

// Ported from tests/test_policy.py.
const now = 1_000_000.0

func handoff(status, agent string) model.HandoffPayload {
	return model.HandoffPayload{TaskID: "TASK-1", AgentID: agent, Status: status, Timestamp: now}
}

func heldBy(agent string, expiresIn float64) map[string]any {
	return map[string]any{"task_id": "TASK-1", "agent_id": agent, "claimed_at": now - 60, "expires_at": now + expiresIn}
}

func TestCompleteFromTheLockHolderShips(t *testing.T) {
	if !ShouldShip(handoff("complete", "claude"), heldBy("claude", 600)) {
		t.Fatal("finished work from the holder must ship")
	}
}

func TestPausedNeverShips(t *testing.T) {
	if ShouldShip(handoff("paused", "claude"), heldBy("claude", 600)) {
		t.Fatal("paused shipped")
	}
}

func TestFailedNeverShips(t *testing.T) {
	if ShouldShip(handoff("failed", "claude"), heldBy("claude", 600)) {
		t.Fatal("failed shipped")
	}
}

func TestUnknownStatusNeverShips(t *testing.T) {
	if ShouldShip(handoff("unknown", "claude"), heldBy("claude", 600)) {
		t.Fatal("unknown shipped")
	}
}

func TestCompleteWithoutAHeldLockDoesNotShip(t *testing.T) {
	if ShouldShip(handoff("complete", "claude"), nil) {
		t.Fatal("shipped with no lock")
	}
}

func TestCompleteFromAnotherAgentDoesNotShip(t *testing.T) {
	if ShouldShip(handoff("complete", "codex"), heldBy("claude", 600)) {
		t.Fatal("shipped another agent's lock")
	}
}

func TestCompleteAfterLeaseExpiredDoesNotShip(t *testing.T) {
	if ShouldShip(handoff("complete", "claude"), heldBy("claude", -1)) {
		t.Fatal("shipped after the lease expired")
	}
}

func TestLockMissingExpiryDoesNotShip(t *testing.T) {
	lock := heldBy("claude", 600)
	delete(lock, "expires_at")
	if ShouldShip(handoff("complete", "claude"), lock) {
		t.Fatal("shipped a lock with no expiry")
	}
}

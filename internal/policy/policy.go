// Package policy decides whether a handoff may be verified, committed and pushed.
// It is a direct port of orchestra/policy.py::should_ship.
package policy

import "github.com/Adeel0-0/agent-orchestra/internal/model"

// ShouldShip returns true only for finished work released by the agent that held
// the writer lock, before that lock's lease expired:
//
//   - status is "complete"
//   - a lock was held at release time (releasedLock is not nil)
//   - releasedLock["agent_id"] equals the handoff's agent
//   - releasedLock["expires_at"] (missing means 0) >= the handoff's timestamp
//
// Any missing field fails closed.
func ShouldShip(handoff model.HandoffPayload, releasedLock map[string]any) bool {
	if handoff.Status != "complete" {
		return false
	}
	if len(releasedLock) == 0 {
		return false
	}
	if releasedLock["agent_id"] != any(handoff.AgentID) {
		return false
	}
	if model.LockExpiresAt(releasedLock) < handoff.Timestamp {
		return false
	}
	return true
}

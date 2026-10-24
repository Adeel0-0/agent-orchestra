"""
Handoff Shipping Policy.
Decides whether a HANDOFF_REQUEST may trigger verification, auto-commit, and push.
The daemon calls this once per handoff event; returning False leaves the work untouched.
"""

from __future__ import annotations
from typing import Any, Dict, Optional

from .models import HandoffPayload


def should_ship(handoff: HandoffPayload, released_lock: Optional[Dict[str, Any]]) -> bool:
    """
    Return True only if this handoff is finished work that may be verified, committed, and pushed.

    handoff:
        task_id, agent_id (who sent the handoff), summary, timestamp (when it was sent), and
        status: "complete" | "paused" | "failed" | "unknown" (missing or unrecognized).
    released_lock:
        The writer lock as it stood when the agent released it, or None if no lock was held.
        Keys: task_id, agent_id, scope, claimed_at, expires_at (epoch seconds), pid.
    """
    # TODO(owner): implement the shipping policy. Questions to settle:
    #   - Only "complete" should ever ship; "paused", "failed", and "unknown" must not.
    #   - Ship if the agent never held the lock (released_lock is None)?
    #   - Ship if released_lock["agent_id"] differs from handoff.agent_id (a --force release)?
    #   - Ship if the lease had already expired (released_lock["expires_at"] < handoff.timestamp)?
    #     Another agent could have claimed and written in the meantime.
    return False

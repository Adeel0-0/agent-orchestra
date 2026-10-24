import time
import unittest

from orchestra.models import HandoffPayload
from orchestra.policy import should_ship


def handoff(status, agent_id="claude"):
    return HandoffPayload(task_id="TASK-1", agent_id=agent_id, status=status, summary="", timestamp=time.time())


def held_lock(agent_id="claude"):
    now = time.time()
    return {"task_id": "TASK-1", "agent_id": agent_id, "scope": "", "claimed_at": now - 60, "expires_at": now + 600}


class TestShouldShip(unittest.TestCase):
    """The non-negotiable cases. Add tests for the edge cases your policy decides."""

    def test_complete_from_the_lock_holder_ships(self):
        self.assertTrue(should_ship(handoff("complete"), held_lock()))

    def test_paused_never_ships(self):
        self.assertFalse(should_ship(handoff("paused"), held_lock()))

    def test_failed_never_ships(self):
        self.assertFalse(should_ship(handoff("failed"), held_lock()))

    def test_unknown_status_never_ships(self):
        self.assertFalse(should_ship(handoff("unknown"), held_lock()))


if __name__ == "__main__":
    unittest.main()

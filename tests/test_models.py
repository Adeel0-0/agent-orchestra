import unittest
from orchestra.models import AgentEvent, EventType, TaskClaim, HandoffPayload, ReviewVerdict

class TestModels(unittest.TestCase):
    def test_agent_event_serialization(self):
        evt = AgentEvent(
            event_id="evt_123",
            event_type=EventType.AGENT_CLAIM,
            source_agent="codex",
            payload={"task_id": "TEST-001", "scope": "unit testing"},
        )
        json_str = evt.to_json()
        reconstituted = AgentEvent.from_json(json_str)

        self.assertEqual(reconstituted.event_id, "evt_123")
        self.assertEqual(reconstituted.event_type, EventType.AGENT_CLAIM)
        self.assertEqual(reconstituted.source_agent, "codex")
        self.assertEqual(reconstituted.payload["task_id"], "TEST-001")

    def test_task_claim(self):
        claim = TaskClaim(
            task_id="AUTH-001",
            agent_id="claude",
            base_commit="abc1234",
            scope_description="Login refactor",
        )
        d = claim.to_dict()
        self.assertEqual(d["task_id"], "AUTH-001")
        self.assertEqual(d["agent_id"], "claude")

        claim2 = TaskClaim.from_dict(d)
        self.assertEqual(claim2.base_commit, "abc1234")

    def test_review_verdict(self):
        verdict = ReviewVerdict(
            task_id="AUTH-001",
            reviewer_agent_id="codex",
            accepted=True,
            comments="Looks solid",
            findings=["Small typo on line 12"],
        )
        d = verdict.to_dict()
        self.assertTrue(d["accepted"])
        self.assertEqual(len(d["findings"]), 1)

if __name__ == "__main__":
    unittest.main()


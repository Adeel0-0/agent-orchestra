import shutil
import tempfile
import unittest
from pathlib import Path

from orchestra.bus import EventBus, LockManager
from orchestra.models import EventType

class TestBusAndLock(unittest.TestCase):
    def setUp(self):
        self.temp_dir = Path(tempfile.mkdtemp())

    def tearDown(self):
        shutil.rmtree(self.temp_dir, ignore_errors=True)

    def test_event_bus_emit_and_read(self):
        bus = EventBus(self.temp_dir)
        e1 = bus.emit(EventType.AGENT_CLAIM, "claude", {"task_id": "T1"})
        e2 = bus.emit(EventType.HANDOFF_REQUEST, "claude", {"task_id": "T1"})

        events = bus.read_events()
        self.assertEqual(len(events), 2)
        self.assertEqual(events[0].event_type, EventType.AGENT_CLAIM)
        self.assertEqual(events[1].event_type, EventType.HANDOFF_REQUEST)

        latest = bus.get_latest_event()
        self.assertIsNotNone(latest)
        self.assertEqual(latest.event_id, e2.event_id)

    def test_lock_manager_single_writer(self):
        locks = LockManager(self.temp_dir)
        self.assertFalse(locks.is_locked())

        # Claude acquires
        ok = locks.acquire("TASK-1", "claude", lease_seconds=100)
        self.assertTrue(ok)
        self.assertTrue(locks.is_locked())

        # Codex attempts to acquire without force -> fails
        ok2 = locks.acquire("TASK-2", "codex", lease_seconds=100, force=False)
        self.assertFalse(ok2)

        # Codex attempts to release Claude's lock without force -> fails
        rel = locks.release("codex", force=False)
        self.assertFalse(rel)
        self.assertTrue(locks.is_locked())

        # Claude releases
        rel_claude = locks.release("claude", force=False)
        self.assertTrue(rel_claude)
        self.assertFalse(locks.is_locked())

        # Now Codex can acquire
        ok3 = locks.acquire("TASK-2", "codex", lease_seconds=100)
        self.assertTrue(ok3)
        self.assertTrue(locks.is_locked())

if __name__ == "__main__":
    unittest.main()


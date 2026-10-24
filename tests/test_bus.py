import os
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

from orchestra.bus import EventBus, LockManager
from orchestra.models import EventType

REPO_ROOT = Path(__file__).resolve().parent.parent

# Each worker spins until a shared start time, then races to acquire the same lock.
RACE_WORKER = """
import sys, time
from orchestra.bus import LockManager
orchestra_dir, agent, start_at = sys.argv[1], sys.argv[2], float(sys.argv[3])
while time.time() < start_at:
    pass
print(LockManager(orchestra_dir).acquire("RACE", agent, lease_seconds=100))
"""

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

    def test_read_from_leaves_half_written_line_for_next_read(self):
        bus = EventBus(self.temp_dir)
        offset = bus.end_offset()
        bus.emit(EventType.AGENT_CLAIM, "claude", {"task_id": "T1"})
        with open(bus.events_file, "a", encoding="utf-8") as f:
            f.write('{"event_id": "evt_partial", "event_type": "HANDOFF_REQ')  # writer mid-line

        events, offset = bus.read_from(offset)
        self.assertEqual([e.event_type for e in events], [EventType.AGENT_CLAIM])

        with open(bus.events_file, "a", encoding="utf-8") as f:
            f.write('UEST", "source_agent": "claude", "payload": {}}\n')
        events, _ = bus.read_from(offset)
        self.assertEqual([e.event_id for e in events], ["evt_partial"])

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

    def test_lock_has_one_owner_when_processes_race(self):
        env = {**os.environ, "PYTHONPATH": str(REPO_ROOT)}
        start_at = time.time() + 2.0  # leaves time for every interpreter to boot
        workers = [
            subprocess.Popen(
                [sys.executable, "-c", RACE_WORKER, str(self.temp_dir), f"agent{i}", str(start_at)],
                env=env,
                stdout=subprocess.PIPE,
                text=True,
            )
            for i in range(8)
        ]
        results = [w.communicate(timeout=60)[0].strip() for w in workers]

        self.assertEqual(results.count("True"), 1, f"worker results: {results}")
        winner = f"agent{results.index('True')}"
        self.assertEqual(LockManager(self.temp_dir).get_current_lock()["agent_id"], winner)

if __name__ == "__main__":
    unittest.main()


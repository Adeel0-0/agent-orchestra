"""Regression tests for the Phase 0 hardening fixes."""

import json
import subprocess
import sys
import tempfile
import threading
import unittest
from pathlib import Path

from orchestra.bus import EventBus, LockManager
from orchestra.config import OrchestraConfig
from orchestra.daemon import OrchestraDaemon, should_run_pipeline
from orchestra.git_sync import GitEngine
from orchestra.models import EventType, OrchestraState

REPO_ROOT = Path(__file__).resolve().parent.parent


class TestEntryPoint(unittest.TestCase):
    def test_python_dash_m_runs(self):
        with tempfile.TemporaryDirectory() as tmp:
            res = subprocess.run(
                [sys.executable, "-m", "orchestra", "status", "--json"],
                cwd=tmp,
                env={**__import__("os").environ, "PYTHONPATH": str(REPO_ROOT)},
                capture_output=True,
                text=True,
                timeout=60,
            )
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertIn("state", json.loads(res.stdout))

    def test_importing_cli_does_not_run_it(self):
        res = subprocess.run(
            [sys.executable, "-c", "import orchestra.cli; print('imported')"],
            cwd=REPO_ROOT,
            capture_output=True,
            text=True,
            timeout=60,
        )
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(res.stdout.strip(), "imported")


class TestLockRace(unittest.TestCase):
    def test_only_one_of_many_simultaneous_claims_wins(self):
        with tempfile.TemporaryDirectory() as tmp:
            start = threading.Barrier(16)
            wins = []

            def claim(i: int) -> None:
                locks = LockManager(tmp)
                start.wait()
                if locks.acquire(task_id="T", agent_id=f"agent-{i}"):
                    wins.append(i)

            threads = [threading.Thread(target=claim, args=(i,)) for i in range(16)]
            for t in threads:
                t.start()
            for t in threads:
                t.join()

            self.assertEqual(len(wins), 1)
            self.assertEqual(LockManager(tmp).get_current_lock()["agent_id"], f"agent-{wins[0]}")
            self.assertFalse((Path(tmp) / "writer.lock.guard").exists())

    def test_expired_lock_can_be_taken(self):
        with tempfile.TemporaryDirectory() as tmp:
            locks = LockManager(tmp)
            self.assertTrue(locks.acquire("T1", "a", lease_seconds=-1))
            self.assertTrue(locks.acquire("T2", "b"))
            self.assertEqual(locks.get_current_lock()["agent_id"], "b")


class TestEventLog(unittest.TestCase):
    def test_read_from_returns_only_new_complete_events(self):
        with tempfile.TemporaryDirectory() as tmp:
            bus = EventBus(tmp)
            bus.emit(EventType.CUSTOM_EVENT, "a", {"n": 1})
            events, offset = bus.read_from(0)
            self.assertEqual([e.payload["n"] for e in events], [1])

            bus.emit(EventType.CUSTOM_EVENT, "a", {"n": 2})
            with open(bus.events_file, "a", encoding="utf-8") as f:
                f.write('{"event_id": "half')  # a writer caught mid-line
            events, offset2 = bus.read_from(offset)
            self.assertEqual([e.payload["n"] for e in events], [2])
            self.assertEqual(bus.read_from(offset2), ([], offset2))

    def test_tail_read_and_rotation(self):
        with tempfile.TemporaryDirectory() as tmp:
            bus = EventBus(tmp, max_bytes=2000)
            for n in range(60):
                bus.emit(EventType.CUSTOM_EVENT, "a", {"n": n})
            self.assertTrue(bus.rotated_file.is_file())
            self.assertLess(bus.events_file.stat().st_size, 2500)
            self.assertEqual([e.payload["n"] for e in bus.read_events(limit=2)], [58, 59])
            # A reader holding an offset from before the rotation restarts cleanly.
            events, _ = bus.read_from(10**9)
            self.assertEqual(events[-1].payload["n"], 59)


class TestDaemonPipeline(unittest.TestCase):
    def _daemon(self, tmp: str, exit_code: int = 0) -> OrchestraDaemon:
        root = Path(tmp)
        (root / "verify.py").write_text(
            "import sys\nopen('runs.txt', 'a').write('x')\nsys.exit(%d)\n" % exit_code,
            encoding="utf-8",
        )
        cfg = OrchestraConfig(workspace_root=tmp)
        cfg.pipeline.verify_command = "python verify.py"
        cfg.git.enabled = False
        return OrchestraDaemon(cfg)

    @staticmethod
    def _runs(tmp: str) -> int:
        f = Path(tmp) / "runs.txt"
        return len(f.read_text()) if f.is_file() else 0

    def test_policy(self):
        self.assertTrue(should_run_pipeline({"status": "complete"}))
        self.assertTrue(should_run_pipeline({"note": "no status"}))
        self.assertFalse(should_run_pipeline({"status": "paused"}))
        self.assertFalse(should_run_pipeline({"status": "failed"}))

    def test_no_handoff_means_no_pipeline(self):
        with tempfile.TemporaryDirectory() as tmp:
            daemon = self._daemon(tmp)
            (Path(tmp) / "half_typed.py").write_text("x = ", encoding="utf-8")
            for _ in range(3):
                daemon.tick()
            self.assertEqual(self._runs(tmp), 0)

    def test_handoff_runs_pipeline_exactly_once(self):
        with tempfile.TemporaryDirectory() as tmp:
            daemon = self._daemon(tmp)
            daemon.bus.emit(EventType.HANDOFF_REQUEST, "a", {"task_id": "T", "status": "complete"})
            for _ in range(3):
                daemon.tick()
            self.assertEqual(self._runs(tmp), 1)
            self.assertEqual(daemon.current_state, OrchestraState.IDLE_CLEAN)

    def test_paused_handoff_is_ignored(self):
        with tempfile.TemporaryDirectory() as tmp:
            daemon = self._daemon(tmp)
            daemon.bus.emit(EventType.HANDOFF_REQUEST, "a", {"task_id": "T", "status": "paused"})
            daemon.tick()
            self.assertEqual(self._runs(tmp), 0)

    def test_pipeline_waits_while_locked(self):
        with tempfile.TemporaryDirectory() as tmp:
            daemon = self._daemon(tmp)
            daemon.bus.emit(EventType.HANDOFF_REQUEST, "a", {"status": "complete"})
            daemon.locks.acquire("T2", "b")
            daemon.tick()
            self.assertEqual(self._runs(tmp), 0)
            daemon.locks.release(force=True)
            daemon.tick()
            self.assertEqual(self._runs(tmp), 1)

    def test_failed_verify_holds_error_until_reset(self):
        with tempfile.TemporaryDirectory() as tmp:
            daemon = self._daemon(tmp, exit_code=1)
            daemon.bus.emit(EventType.HANDOFF_REQUEST, "a", {"status": "complete"})
            for _ in range(3):
                daemon.tick()
            self.assertEqual(self._runs(tmp), 1)
            self.assertEqual(daemon.current_state, OrchestraState.ERROR)

            # A second process (e.g. `orchestra status`) sees the held error.
            other = OrchestraDaemon(daemon.config)
            self.assertEqual(other.get_status()["state"], "ERROR")
            self.assertIn("Verification failed", other.get_status()["last_error"])

            other.reset()
            self.assertEqual(OrchestraDaemon(daemon.config).get_status()["state"], "IDLE_CLEAN")

    def test_old_handoffs_are_not_replayed_on_first_start(self):
        with tempfile.TemporaryDirectory() as tmp:
            EventBus(Path(tmp) / ".orchestra").emit(EventType.HANDOFF_REQUEST, "a", {"status": "complete"})
            daemon = self._daemon(tmp)
            daemon.tick()
            self.assertEqual(self._runs(tmp), 0)


class TestConfig(unittest.TestCase):
    def test_push_is_opt_in_and_unknown_keys_are_ignored(self):
        self.assertFalse(OrchestraConfig().git.auto_push)
        cfg = OrchestraConfig.from_dict({"git": {"auto_push": True, "from_a_newer_version": 1}})
        self.assertTrue(cfg.git.auto_push)


class TestGitFastForward(unittest.TestCase):
    def _git(self, cwd: Path, *args: str) -> str:
        res = subprocess.run(
            ["git", "-c", "user.name=t", "-c", "user.email=t@example.com", *args],
            cwd=cwd,
            capture_output=True,
            text=True,
            check=True,
        )
        return res.stdout.strip()

    def test_fast_forward_does_not_switch_branches(self):
        with tempfile.TemporaryDirectory() as tmp:
            remote = Path(tmp) / "remote.git"
            work = Path(tmp) / "work"
            self._git(Path(tmp), "init", "--bare", "-b", "main", str(remote))
            self._git(Path(tmp), "init", "-b", "main", str(work))
            self._git(work, "remote", "add", "origin", str(remote))
            (work / "a.txt").write_text("1", encoding="utf-8")
            self._git(work, "add", ".")
            self._git(work, "commit", "-m", "one")
            self._git(work, "branch", "secondary")
            (work / "a.txt").write_text("2", encoding="utf-8")
            self._git(work, "commit", "-am", "two")
            (work / "uncommitted.txt").write_text("agent is mid-edit", encoding="utf-8")

            self.assertTrue(GitEngine(work).fast_forward_branch("secondary", "main", "origin"))

            self.assertEqual(self._git(work, "rev-parse", "--abbrev-ref", "HEAD"), "main")
            self.assertTrue((work / "uncommitted.txt").is_file())
            head = self._git(work, "rev-parse", "main")
            self.assertEqual(self._git(work, "rev-parse", "secondary"), head)
            self.assertEqual(self._git(remote, "rev-parse", "secondary"), head)


if __name__ == "__main__":
    unittest.main()

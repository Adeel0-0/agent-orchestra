import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from orchestra.bus import EventBus
from orchestra.config import OrchestraConfig
from orchestra.daemon import OrchestraDaemon
from orchestra.models import EventType, OrchestraState

REPO_ROOT = Path(__file__).resolve().parent.parent

# Passes only while the daemon itself holds the writer lock.
VERIFY_LOCK_HELD = (
    f'"{sys.executable}" -c "import json,sys; '
    "sys.exit(json.load(open('.orchestra/writer.lock'))['agent_id'] != 'orchestra')\""
)
VERIFY_FAILS = f'"{sys.executable}" -c "raise SystemExit(1)"'


def git(repo, *args):
    return subprocess.run(["git", *args], cwd=repo, capture_output=True, text=True, check=True).stdout.strip()


def orchestra_cli(repo, *args):
    """Run the real CLI in `repo`, the way an agent would."""
    return subprocess.run(
        [sys.executable, "-c", "import sys; from orchestra.cli import main; sys.exit(main(sys.argv[1:]))", *args],
        cwd=repo,
        env={**os.environ, "PYTHONPATH": str(REPO_ROOT)},
        capture_output=True,
        text=True,
        check=True,
    )


class TestHandoffDrivenPipeline(unittest.TestCase):
    def setUp(self):
        self.repo = Path(tempfile.mkdtemp())
        git(self.repo, "init", "-q", "-b", "main")
        git(self.repo, "config", "user.email", "test@example.com")
        git(self.repo, "config", "user.name", "test")
        (self.repo / ".gitignore").write_text(".orchestra/\n")
        git(self.repo, "add", ".")
        git(self.repo, "commit", "-q", "-m", "init")
        self.base_sha = git(self.repo, "rev-parse", "HEAD")

    def tearDown(self):
        shutil.rmtree(self.repo, ignore_errors=True)

    def make_daemon(self, verify_command=VERIFY_LOCK_HELD):
        cfg = OrchestraConfig(workspace_root=str(self.repo))
        cfg.git.auto_push = False
        cfg.pipeline.verify_command = verify_command
        return OrchestraDaemon(cfg)

    def agent_finishes(self, status):
        (self.repo / "feature.py").write_text("def feature(): ...\n")
        orchestra_cli(self.repo, "claim", "TASK-1", "--agent", "claude")
        orchestra_cli(self.repo, "release", "--agent", "claude", "--status", status)

    def events_of(self, daemon, event_type):
        return [e for e in daemon.bus.read_events(limit=1000) if e.event_type == event_type]

    def test_dirty_tree_without_handoff_runs_nothing(self):
        daemon = self.make_daemon()
        (self.repo / "wip.py").write_text("x = 1\n")

        daemon.tick()
        daemon.tick()

        self.assertEqual(self.events_of(daemon, EventType.VERIFICATION_STARTED), [])
        self.assertEqual(git(self.repo, "rev-parse", "HEAD"), self.base_sha)
        self.assertEqual(daemon.current_state, OrchestraState.READY_NEEDS_VERIFY)

    def test_paused_handoff_is_never_committed(self):
        daemon = self.make_daemon()
        self.agent_finishes("paused")

        daemon.tick()

        self.assertEqual(self.events_of(daemon, EventType.VERIFICATION_STARTED), [])
        self.assertEqual(git(self.repo, "rev-parse", "HEAD"), self.base_sha)
        self.assertEqual(daemon.current_state, OrchestraState.PAUSED_CHECKPOINT)
        skipped = [e.payload for e in self.events_of(daemon, EventType.CUSTOM_EVENT)]
        self.assertIn({"action": "handoff_skipped", "reason": "policy", "task_id": "TASK-1", "status": "paused"}, skipped)

    @mock.patch("orchestra.daemon.should_ship", return_value=True)
    def test_shippable_handoff_runs_pipeline_once_under_daemon_lock(self, should_ship):
        daemon = self.make_daemon()
        self.agent_finishes("complete")

        daemon.tick()
        daemon.tick()

        handoff, released_lock = should_ship.call_args.args
        self.assertEqual((handoff.task_id, handoff.agent_id, handoff.status), ("TASK-1", "claude", "complete"))
        self.assertEqual(released_lock["agent_id"], "claude")

        self.assertEqual(len(self.events_of(daemon, EventType.VERIFICATION_STARTED)), 1)
        self.assertNotEqual(git(self.repo, "rev-parse", "HEAD"), self.base_sha)
        self.assertIn("feature.py", git(self.repo, "show", "--name-only", "--format=", "HEAD"))
        self.assertFalse(daemon.locks.is_locked())

    @mock.patch("orchestra.daemon.should_ship", return_value=True)
    def test_failed_verification_is_not_retried_every_tick(self, _):
        daemon = self.make_daemon(verify_command=VERIFY_FAILS)
        self.agent_finishes("complete")

        for _ in range(3):
            daemon.tick()

        self.assertEqual(len(self.events_of(daemon, EventType.VERIFICATION_STARTED)), 1)
        self.assertEqual(daemon.current_state, OrchestraState.ERROR)
        self.assertEqual(git(self.repo, "rev-parse", "HEAD"), self.base_sha)

    @mock.patch("orchestra.daemon.should_ship", return_value=True)
    def test_handoff_skipped_when_another_writer_claimed_first(self, _):
        daemon = self.make_daemon()
        self.agent_finishes("complete")
        orchestra_cli(self.repo, "claim", "TASK-2", "--agent", "codex")

        daemon.tick()

        self.assertEqual(self.events_of(daemon, EventType.VERIFICATION_STARTED), [])
        self.assertEqual(git(self.repo, "rev-parse", "HEAD"), self.base_sha)
        self.assertEqual(daemon.locks.get_current_lock()["agent_id"], "codex")

    @mock.patch("orchestra.daemon.should_ship", return_value=True)
    def test_first_start_does_not_replay_old_handoffs(self, _):
        self.agent_finishes("complete")
        daemon = self.make_daemon()

        daemon.tick()

        self.assertEqual(self.events_of(daemon, EventType.VERIFICATION_STARTED), [])

    @mock.patch("orchestra.daemon.should_ship", return_value=True)
    def test_handoff_sent_while_daemon_down_runs_on_restart(self, _):
        self.make_daemon()._save_cursor()  # what start() records before its loop
        self.agent_finishes("complete")

        restarted = self.make_daemon()
        restarted.tick()

        self.assertEqual(len(self.events_of(restarted, EventType.VERIFICATION_STARTED)), 1)


if __name__ == "__main__":
    unittest.main()

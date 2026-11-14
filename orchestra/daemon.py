"""
Central Orchestra Daemon and State Machine.
Acts as the conductor: reacts in real-time to agent actions, enforces single-writer rules,
triggers verification pipelines, executes Python scripts, and handles Git synchronizations.
"""

from __future__ import annotations
import json
import re
import time
from pathlib import Path
from typing import Any, Dict, List, Optional

from .bus import EventBus, LockManager
from .ci_tracker import CiTracker
from .config import OrchestraConfig, load_config
from .git_sync import GitEngine
from .models import EventType, OrchestraState
from .runner import ScriptRunner
from .watcher import FilesystemWatcher


def should_run_pipeline(handoff: Dict[str, Any]) -> bool:
    """
    Decide whether a HANDOFF_REQUEST payload should trigger verify + git sync.

    This is the policy knob for the whole daemon. Default: only a finished task.
    A handoff with no status (for example one synthesized from the coordination
    file) counts as finished. "paused" and "failed" leave the tree alone.
    """
    return str(handoff.get("status", "complete")).lower() == "complete"


class OrchestraDaemon:
    """The central coordinator managing multi-agent collaboration."""

    def __init__(self, config: Optional[OrchestraConfig] = None) -> None:
        self.config = config or load_config()
        self.root = Path(self.config.workspace_root).resolve()
        self.orchestra_dir = self.root / self.config.orchestra_dir
        self.orchestra_dir.mkdir(parents=True, exist_ok=True)

        self.bus = EventBus(self.orchestra_dir)
        self.locks = LockManager(self.orchestra_dir)
        self.runner = ScriptRunner(default_cwd=self.root)
        self.git = GitEngine(self.root)
        self.ci = CiTracker(self.root)

        self.state_file = self.orchestra_dir / "daemon_state.json"
        self.current_state = OrchestraState.IDLE_CLEAN
        self.last_error: Optional[str] = None
        self._pending_handoff: Optional[Dict[str, Any]] = None
        # Only react to handoffs that happen after the daemon first sees this workspace.
        self._events_offset = self.bus.current_offset()
        self._load_state()

        self._watcher: Optional[FilesystemWatcher] = None
        self._running = False

    # ------------------------------------------------------------------ state

    def _load_state(self) -> None:
        try:
            data = json.loads(self.state_file.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return
        if not isinstance(data, dict):
            return
        try:
            self.current_state = OrchestraState(data.get("state"))
        except ValueError:
            pass
        self.last_error = data.get("last_error")
        self._pending_handoff = data.get("pending_handoff")
        if isinstance(data.get("events_offset"), int):
            self._events_offset = data["events_offset"]

    def _save_state(self) -> None:
        tmp = self.state_file.with_suffix(".tmp")
        try:
            tmp.write_text(
                json.dumps(
                    {
                        "state": self.current_state.value,
                        "last_error": self.last_error,
                        "pending_handoff": self._pending_handoff,
                        "events_offset": self._events_offset,
                    },
                    indent=2,
                ),
                encoding="utf-8",
            )
            tmp.replace(self.state_file)
        except OSError:
            pass

    def reset(self) -> None:
        """Clear a held ERROR and any pending handoff."""
        self.current_state = OrchestraState.IDLE_CLEAN
        self.last_error = None
        self._pending_handoff = None
        self._events_offset = self.bus.current_offset()
        self._save_state()

    def _fail(self, message: str) -> None:
        self.current_state = OrchestraState.ERROR
        self.last_error = message
        if self.config.pipeline.on_error_command:
            self.runner.run_command(self.config.pipeline.on_error_command)

    def _ingest_events(self) -> None:
        """Advance through new bus events and remember the newest qualifying handoff."""
        events, self._events_offset = self.bus.read_from(self._events_offset)
        for evt in events:
            if evt.event_type == EventType.HANDOFF_REQUEST and should_run_pipeline(evt.payload):
                self._pending_handoff = {"event_id": evt.event_id, **evt.payload}

    def get_status(self) -> Dict[str, Any]:
        """Inspect the current full operational state."""
        lock = self.locks.get_current_lock()
        is_locked = self.locks.is_locked()
        git_status = self.git.get_status() if self.git.is_git_repo() else None
        latest = self.bus.get_latest_event()
        state = OrchestraState.WRITER_ACTIVE if is_locked else self.current_state

        return {
            "state": state.value,
            "last_error": self.last_error,
            "is_locked": is_locked,
            "active_lock": lock,
            "git": {
                "branch": git_status.branch,
                "head_sha": git_status.head_sha,
                "is_clean": git_status.is_clean,
                "changed_count": len(git_status.changed_files),
            }
            if git_status
            else None,
            "latest_event": latest.to_dict() if latest else None,
        }

    # ---------------------------------------------------------------- watcher

    def on_filesystem_change(self, changed_paths: List[Path]) -> None:
        """Called automatically by FilesystemWatcher when files are created/modified."""
        rel_paths = [str(p.relative_to(self.root)) for p in changed_paths if p.is_relative_to(self.root)]

        lock = self.locks.get_current_lock()
        source = lock.get("agent_id", "unknown") if lock else "untracked"

        # Emit activity event
        self.bus.emit(
            event_type=EventType.TASK_PROGRESS,
            source_agent=source,
            payload={
                "changed_count": len(rel_paths),
                "sample_files": rel_paths[:5],
                "task_id": lock.get("task_id") if lock else None,
            },
        )

        # Check if coordination file was modified
        if self.config.coordination_file:
            coord_norm = self.config.coordination_file.replace("\\", "/")
            for p in rel_paths:
                if p.replace("\\", "/") == coord_norm:
                    self._check_coordination_file()
                    break

    def _check_coordination_file(self) -> None:
        """Inspect external coordination file (e.g. docs/coordination.md) if configured."""
        if not self.config.coordination_file:
            return
        coord_path = self.root / self.config.coordination_file
        if not coord_path.is_file():
            return

        try:
            content = coord_path.read_text(encoding="utf-8")
            # Check for active writer lease
            m = re.search(r"-\s+\*\*Active writer:\*\*\s*(.+)$", content, re.MULTILINE)
            if m:
                writer_line = m.group(1).strip()
                if writer_line.lower().startswith("none"):
                    if self.locks.is_locked():
                        current = self.locks.get_current_lock()
                        self.locks.release(force=True)
                        self.bus.emit(
                            event_type=EventType.HANDOFF_REQUEST,
                            source_agent=current.get("agent_id", "agent") if current else "agent",
                            payload={"note": "Writer released via coordination file"},
                        )
                else:
                    # Writer is active
                    agent_name = writer_line.split(",")[0].strip()
                    if not self.locks.is_locked():
                        self.locks.acquire(task_id="auto_task", agent_id=agent_name, scope=writer_line)
                        self.bus.emit(
                            event_type=EventType.AGENT_CLAIM,
                            source_agent=agent_name,
                            payload={"scope": writer_line},
                        )
        except Exception:
            pass

    # ---------------------------------------------------------- state machine

    def tick(self) -> Dict[str, Any]:
        """
        Execute a single evaluation pass of the state machine.

        The pipeline runs once per HANDOFF_REQUEST, never merely because the tree
        is dirty: a human or agent may be mid-edit without holding the lock.
        """
        self._ingest_events()

        # 1. A writer holds the lock: wait. A pending handoff stays pending.
        if self.locks.is_locked():
            self.current_state = OrchestraState.WRITER_ACTIVE
            self._save_state()
            return self.get_status()

        # 2. A handoff arrived: run the pipeline exactly once for it.
        if self._pending_handoff is not None:
            self._pending_handoff = None
            self._save_state()
            self.run_handoff_pipeline()
            self._save_state()
            return self.get_status()

        # 3. A failure is held until the next handoff, a new claim, or `orchestra reset`.
        if self.current_state != OrchestraState.ERROR:
            git_stat = self.git.get_status() if self.git.is_git_repo() else None
            dirty = bool(git_stat and not git_stat.is_clean)
            self.current_state = OrchestraState.READY_NEEDS_VERIFY if dirty else OrchestraState.IDLE_CLEAN
        self._save_state()
        return self.get_status()

    def run_handoff_pipeline(self) -> bool:
        """
        Executes verification, auto-commit, branch synchronization, and post-sync hooks.
        """
        if not self.config.pipeline.verify_command:
            self.current_state = OrchestraState.IDLE_CLEAN
            return True

        self.last_error = None
        self.current_state = OrchestraState.VERIFYING
        self.bus.emit(
            event_type=EventType.VERIFICATION_STARTED,
            source_agent="orchestra",
            payload={"command": self.config.pipeline.verify_command},
        )

        res = self.runner.run_command(self.config.pipeline.verify_command)
        self.bus.emit(
            event_type=EventType.VERIFICATION_RESULT,
            source_agent="orchestra",
            payload=res.to_dict(),
        )

        if not res.success:
            self._fail(f"Verification failed (exit {res.exit_code}): {self.config.pipeline.verify_command}")
            return False

        # Verification succeeded! Proceed to Git sync
        self.current_state = OrchestraState.SYNCING
        if self.config.git.enabled and self.git.is_git_repo():
            error = self._execute_git_sync()
            if error:
                self.bus.emit(
                    event_type=EventType.GIT_ERROR,
                    source_agent="orchestra",
                    payload={"error": error},
                )
                self._fail(error)
                return False

        # Run post-sync command
        if self.config.pipeline.post_sync_command:
            self.runner.run_command(self.config.pipeline.post_sync_command)

        self.current_state = OrchestraState.IDLE_CLEAN
        return True

    def _execute_git_sync(self) -> Optional[str]:
        """
        Stage, commit, rebase, push and fast-forward configured branches.
        Returns None on success, or a message naming the step that failed.
        Every step stops the sequence when it fails; nothing is pushed after a failure.
        """
        cfg = self.config.git
        stat = self.git.get_status()

        if not stat.is_clean and cfg.auto_commit:
            msg = f"{cfg.commit_prefix} automated multi-agent handoff sync\n\nVerified by Agent Orchestra pipeline."
            if not self.git.stage_all() or not self.git.commit(msg):
                return "git commit failed"
            self.bus.emit(
                event_type=EventType.GIT_COMMITTED,
                source_agent="orchestra",
                payload={"commit_message": msg},
            )

        synced: List[str] = []
        pushed = False
        # Remote steps only make sense from the main branch and only when opted in.
        if cfg.auto_push and stat.branch == cfg.main_branch:
            if not self.git.fetch(cfg.remote):
                return f"git fetch {cfg.remote} failed"
            if not self.git.rebase(f"{cfg.remote}/{cfg.main_branch}"):
                self.git.abort_rebase()
                return f"git rebase onto {cfg.remote}/{cfg.main_branch} failed; rebase aborted, nothing pushed"
            if not self.git.push(cfg.remote, cfg.main_branch):
                return f"git push {cfg.remote} {cfg.main_branch} failed"
            pushed = True

            for sec in cfg.sync_branches:
                if not self.git.fast_forward_branch(sec, cfg.main_branch, cfg.remote):
                    return f"could not fast-forward branch '{sec}'"
                synced.append(sec)

        self.bus.emit(
            event_type=EventType.GIT_SYNCED,
            source_agent="orchestra",
            payload={
                "branch": stat.branch,
                "main_branch": cfg.main_branch,
                "pushed": pushed,
                "synced_branches": synced,
            },
        )
        return None

    # -------------------------------------------------------------- lifecycle

    def start(self, poll_interval: float = 3.0) -> None:
        """Start the background filesystem watcher and autonomous event loop."""
        self._running = True
        self._watcher = FilesystemWatcher(
            root_dir=self.root,
            watch_paths=self.config.watch_paths,
            ignore_patterns=self.config.ignore_patterns,
            on_change=self.on_filesystem_change,
            debounce_seconds=self.config.debounce_ms / 1000.0,
        )
        self._watcher.start()

        try:
            while self._running:
                self.tick()
                time.sleep(poll_interval)
        except KeyboardInterrupt:
            self.stop()

    def stop(self) -> None:
        """Gracefully stop the daemon."""
        self._running = False
        if self._watcher:
            self._watcher.stop()

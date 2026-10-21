"""
Central Orchestra Daemon and State Machine.
Acts as the conductor: reacts in real-time to agent actions, enforces single-writer rules,
triggers verification pipelines, executes Python scripts, and handles Git synchronizations.
"""

from __future__ import annotations
import os
import sys
import time
from pathlib import Path
from typing import Any, Dict, List, Optional

from .bus import EventBus, LockManager
from .ci_tracker import CiTracker
from .config import OrchestraConfig, load_config
from .git_sync import GitEngine
from .models import AgentEvent, EventType, OrchestraState
from .runner import ScriptRunner
from .watcher import FilesystemWatcher


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

        self.current_state = OrchestraState.IDLE_CLEAN
        self._watcher: Optional[FilesystemWatcher] = None
        self._running = False

    def get_status(self) -> Dict[str, Any]:
        """Inspect the current full operational state."""
        lock = self.locks.get_current_lock()
        git_status = self.git.get_status() if self.git.is_git_repo() else None

        return {
            "state": self.current_state.value,
            "is_locked": self.locks.is_locked(),
            "active_lock": lock,
            "git": {
                "branch": git_status.branch if git_status else None,
                "head_sha": git_status.head_sha if git_status else None,
                "is_clean": git_status.is_clean if git_status else None,
                "changed_count": len(git_status.changed_files) if git_status else 0,
            }
            if git_status
            else None,
            "latest_event": self.bus.get_latest_event().to_dict() if self.bus.get_latest_event() else None,
        }

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
            import re
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

    def tick(self) -> Dict[str, Any]:
        """
        Execute a single evaluation pass of the state machine.
        Checks locks, git tree, verification triggers, and synchronizations.
        """
        git_stat = self.git.get_status() if self.git.is_git_repo() else None
        lock = self.locks.get_current_lock()
        is_locked = self.locks.is_locked()

        # 1. WRITER_ACTIVE
        if is_locked:
            self.current_state = OrchestraState.WRITER_ACTIVE
            return self.get_status()

        # 2. Check if git tree is dirty and writer is released -> READY_NEEDS_VERIFY
        if git_stat and not git_stat.is_clean:
            self.current_state = OrchestraState.READY_NEEDS_VERIFY
            # Run verification pipeline if configured
            if self.config.pipeline.verify_command:
                self.run_handoff_pipeline()
            return self.get_status()

        # 3. Clean and idle
        self.current_state = OrchestraState.IDLE_CLEAN
        return self.get_status()

    def run_handoff_pipeline(self) -> bool:
        """
        Executes verification, auto-commit, branch synchronization, and post-sync hooks.
        """
        if not self.config.pipeline.verify_command:
            return True

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
            self.current_state = OrchestraState.ERROR
            if self.config.pipeline.on_error_command:
                self.runner.run_command(self.config.pipeline.on_error_command)
            return False

        # Verification succeeded! Proceed to Git sync
        self.current_state = OrchestraState.SYNCING
        if self.config.git.enabled and self.git.is_git_repo():
            self._execute_git_sync()

        # Run post-sync command
        if self.config.pipeline.post_sync_command:
            self.runner.run_command(self.config.pipeline.post_sync_command)

        self.current_state = OrchestraState.IDLE_CLEAN
        return True

    def _execute_git_sync(self) -> None:
        """Stage, commit, rebase, and fast-forward sync configured branches."""
        stat = self.git.get_status()
        if not stat.is_clean and self.config.git.auto_commit:
            self.git.stage_all()
            msg = f"{self.config.git.commit_prefix} automated multi-agent handoff sync\n\nVerified by Agent Orchestra pipeline."
            self.git.commit(msg)
            self.bus.emit(
                event_type=EventType.GIT_COMMITTED,
                source_agent="orchestra",
                payload={"commit_message": msg},
            )

        # Rebase on remote
        self.git.fetch(self.config.git.remote)
        self.git.rebase(f"{self.config.git.remote}/{self.config.git.main_branch}")

        if self.config.git.auto_push:
            self.git.push(self.config.git.remote, self.config.git.main_branch)

        # Fast forward secondary branches
        for sec in self.config.git.sync_branches:
            self.git.fast_forward_branch(
                target_branch=sec,
                source_branch=self.config.git.main_branch,
                remote=self.config.git.remote,
            )

        self.bus.emit(
            event_type=EventType.GIT_SYNCED,
            source_agent="orchestra",
            payload={
                "main_branch": self.config.git.main_branch,
                "synced_branches": self.config.git.sync_branches,
            },
        )

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


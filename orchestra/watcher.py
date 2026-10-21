"""
Real-time Filesystem & Coordination Watcher.
Monitors directory trees and coordination files with debouncing to instantly
detect when an agent begins editing code or changes task ownership.
Zero hard dependencies (uses pure Python polling by default, with optional watchdog support).
"""

from __future__ import annotations
import fnmatch
import os
import threading
import time
from pathlib import Path
from typing import Callable, Dict, List, Optional, Set


class FilesystemWatcher:
    """
    Watches files and directories for modifications, creations, and deletions.
    Calls `on_change(changed_paths)` whenever activity settles (debounced).
    """

    def __init__(
        self,
        root_dir: Path | str,
        watch_paths: List[str],
        ignore_patterns: List[str],
        on_change: Callable[[List[Path]], None],
        debounce_seconds: float = 0.5,
        poll_interval: float = 0.3,
    ) -> None:
        self.root_dir = Path(root_dir).resolve()
        self.watch_paths = [Path(p) if Path(p).is_absolute() else self.root_dir / p for p in watch_paths]
        self.ignore_patterns = ignore_patterns
        self.on_change = on_change
        self.debounce_seconds = debounce_seconds
        self.poll_interval = poll_interval

        self._snapshot: Dict[str, float] = {}
        self._pending_changes: Set[Path] = set()
        self._last_change_time: float = 0.0
        self._stop_event = threading.Event()
        self._thread: Optional[threading.Thread] = None

    def start(self) -> None:
        """Start the background watcher thread."""
        self._snapshot = self._scan_filesystem()
        self._stop_event.clear()
        self._thread = threading.Thread(target=self._run_loop, daemon=True, name="OrchestraWatcherThread")
        self._thread.start()

    def stop(self) -> None:
        """Stop the background watcher."""
        self._stop_event.set()
        if self._thread and self._thread.is_alive():
            self._thread.join(timeout=2.0)

    def _should_ignore(self, rel_path_str: str) -> bool:
        """Test if relative path matches any ignore pattern."""
        normalized = rel_path_str.replace("\\", "/")
        for pat in self.ignore_patterns:
            pat_norm = pat.replace("\\", "/")
            if fnmatch.fnmatch(normalized, pat_norm):
                return True
            if pat_norm.endswith("/*") and normalized.startswith(pat_norm[:-2]):
                return True
        return False

    def _scan_filesystem(self) -> Dict[str, float]:
        """Scan watched directories and record mtime for all matching files."""
        current_state: Dict[str, float] = {}

        for base_path in self.watch_paths:
            if not base_path.exists():
                continue

            if base_path.is_file():
                try:
                    rel = str(base_path.relative_to(self.root_dir))
                    if not self._should_ignore(rel):
                        current_state[rel] = base_path.stat().st_mtime
                except Exception:
                    pass
                continue

            # Directory walk
            for root, dirs, files in os.walk(base_path):
                # Filter out ignored directories in-place to speed up scan
                dirs[:] = [
                    d
                    for d in dirs
                    if not self._should_ignore(str(Path(root, d).relative_to(self.root_dir)))
                ]

                for file in files:
                    file_path = Path(root, file)
                    try:
                        rel = str(file_path.relative_to(self.root_dir))
                        if not self._should_ignore(rel):
                            current_state[rel] = file_path.stat().st_mtime
                    except Exception:
                        continue

        return current_state

    def _run_loop(self) -> None:
        """Main polling loop with debouncing logic."""
        while not self._stop_event.is_set():
            new_snapshot = self._scan_filesystem()

            # Detect differences
            added_or_modified = set()
            for path_str, mtime in new_snapshot.items():
                if path_str not in self._snapshot or self._snapshot[path_str] != mtime:
                    added_or_modified.add(self.root_dir / path_str)

            deleted = set()
            for path_str in self._snapshot:
                if path_str not in new_snapshot:
                    deleted.add(self.root_dir / path_str)

            changed = added_or_modified | deleted
            if changed:
                self._pending_changes.update(changed)
                self._last_change_time = time.time()
                self._snapshot = new_snapshot

            # Check if debounce window expired
            if self._pending_changes and (time.time() - self._last_change_time) >= self.debounce_seconds:
                ready_to_emit = list(self._pending_changes)
                self._pending_changes.clear()
                try:
                    self.on_change(ready_to_emit)
                except Exception:
                    pass

            time.sleep(self.poll_interval)


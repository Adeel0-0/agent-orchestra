"""
File-based IPC Event Bus and Distributed Lock Manager.
Provides reliable asynchronous communication and strict single-writer enforcement
across completely disconnected CLI sessions (Claude Code, Codex, Antigravity, custom scripts).
"""

from __future__ import annotations
import json
import os
import time
import uuid
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Callable, Dict, Generator, Iterator, List, Optional, Tuple

if os.name == "nt":
    import msvcrt
else:
    import fcntl

from .models import AgentEvent, EventType


def _retry_on_sharing_violation(action: Callable[[], None], attempts: int = 5) -> None:
    """
    Windows refuses to replace or delete a file while another process has it open,
    even for the instant a reader needs, so retry briefly before giving up.
    """
    for attempt in range(attempts):
        try:
            action()
            return
        except PermissionError:
            if attempt == attempts - 1:
                raise
            time.sleep(0.05)


class EventBus:
    """
    Append-only file-based event stream backed by JSON Lines (.jsonl).
    Safe for concurrent multi-process access.
    """

    def __init__(self, orchestra_dir: Path | str) -> None:
        self.orchestra_dir = Path(orchestra_dir).resolve()
        self.orchestra_dir.mkdir(parents=True, exist_ok=True)
        self.events_file = self.orchestra_dir / "events.jsonl"
        self.latest_event_file = self.orchestra_dir / "latest_event.json"
        self._subscribers: List[Callable[[AgentEvent], None]] = []

    def emit(
        self,
        event_type: EventType | str,
        source_agent: str,
        payload: Dict[str, Any],
        event_id: Optional[str] = None,
    ) -> AgentEvent:
        """
        Publish an event to the bus.
        Appends to events.jsonl and updates latest_event.json atomically.
        """
        if isinstance(event_type, str):
            try:
                event_type = EventType(event_type)
            except ValueError:
                event_type = EventType.CUSTOM_EVENT

        event = AgentEvent(
            event_id=event_id or f"evt_{uuid.uuid4().hex[:12]}",
            event_type=event_type,
            source_agent=source_agent,
            payload=payload,
            timestamp=time.time(),
        )

        line = json.dumps(event.to_dict()) + "\n"

        # Append to log
        with open(self.events_file, "a", encoding="utf-8") as f:
            f.write(line)

        # Update latest event file atomically
        tmp_latest = self.orchestra_dir / f".latest_{uuid.uuid4().hex[:6]}.tmp"
        try:
            with open(tmp_latest, "w", encoding="utf-8") as f:
                f.write(line)
            tmp_latest.replace(self.latest_event_file)
        except Exception:
            if tmp_latest.is_file():
                tmp_latest.unlink(missing_ok=True)

        # Notify in-process subscribers
        for sub in self._subscribers:
            try:
                sub(event)
            except Exception:
                pass

        return event

    def subscribe(self, callback: Callable[[AgentEvent], None]) -> None:
        """Register an in-process callback for emitted events."""
        self._subscribers.append(callback)

    def read_events(
        self, since_timestamp: float = 0.0, limit: int = 100
    ) -> List[AgentEvent]:
        """Read historical events from the stream."""
        if not self.events_file.is_file():
            return []

        events: List[AgentEvent] = []
        try:
            with open(self.events_file, "r", encoding="utf-8") as f:
                for line in f:
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        evt = AgentEvent.from_json(line)
                        if evt.timestamp >= since_timestamp:
                            events.append(evt)
                    except Exception:
                        continue
        except Exception:
            return []

        return events[-limit:]

    def get_latest_event(self) -> Optional[AgentEvent]:
        """Fetch the most recent event published to the bus."""
        if self.latest_event_file.is_file():
            try:
                with open(self.latest_event_file, "r", encoding="utf-8") as f:
                    return AgentEvent.from_json(f.read().strip())
            except Exception:
                pass

        all_events = self.read_events(limit=1)
        return all_events[0] if all_events else None

    def end_offset(self) -> int:
        """Byte offset of the end of the stream, for readers that want only new events."""
        return self.events_file.stat().st_size if self.events_file.is_file() else 0

    def read_from(self, offset: int) -> Tuple[List[AgentEvent], int]:
        """
        Read events appended after byte `offset`.
        Returns the events and the offset to resume from. A trailing line that another
        process is still writing is left for the next call rather than dropped.
        """
        size = self.end_offset()
        if size < offset:
            offset = 0  # File was truncated/rotated
        if size == offset:
            return [], offset

        with open(self.events_file, "rb") as f:
            f.seek(offset)
            chunk = f.read(size - offset)

        complete_len = chunk.rfind(b"\n") + 1
        events: List[AgentEvent] = []
        for raw in chunk[:complete_len].splitlines():
            line = raw.decode("utf-8", errors="replace").strip()
            if not line:
                continue
            try:
                events.append(AgentEvent.from_json(line))
            except Exception:
                continue
        return events, offset + complete_len

    def stream_events(
        self, poll_interval: float = 0.5, start_from_beginning: bool = False
    ) -> Generator[AgentEvent, None, None]:
        """
        Continuously stream incoming events as a generator.
        Ideal for monitoring CLI commands or reactive daemons.
        """
        offset = 0 if start_from_beginning else self.end_offset()
        while True:
            events, offset = self.read_from(offset)
            yield from events
            time.sleep(poll_interval)


class LockManager:
    """
    Distributed lease-based single-writer lock.
    Prevents two agents from implementing concurrently in the same checkout.
    """

    def __init__(self, orchestra_dir: Path | str) -> None:
        self.orchestra_dir = Path(orchestra_dir).resolve()
        self.orchestra_dir.mkdir(parents=True, exist_ok=True)
        self.lock_file = self.orchestra_dir / "writer.lock"
        self.guard_file = self.orchestra_dir / "writer.lock.guard"

    @contextmanager
    def _guard(self) -> Iterator[None]:
        """
        Hold an OS-level exclusive lock while reading, deciding, and rewriting writer.lock,
        so two agents can never both see "unlocked" and both claim it.
        The OS drops the lock if the holding process dies, so a crash cannot wedge it.
        """
        with open(self.guard_file, "a+b") as fh:
            fh.seek(0)
            if os.name == "nt":
                # LK_LOCK would sleep a full second between its 10 retries, so poll ourselves.
                deadline = time.time() + 10.0
                while True:
                    try:
                        msvcrt.locking(fh.fileno(), msvcrt.LK_NBLCK, 1)
                        break
                    except OSError:
                        if time.time() >= deadline:
                            raise TimeoutError(f"Timed out waiting for {self.guard_file}")
                        time.sleep(0.01)
            else:
                fcntl.flock(fh.fileno(), fcntl.LOCK_EX)
            try:
                yield
            finally:
                fh.seek(0)
                if os.name == "nt":
                    msvcrt.locking(fh.fileno(), msvcrt.LK_UNLCK, 1)
                else:
                    fcntl.flock(fh.fileno(), fcntl.LOCK_UN)

    def _write_lock(self, lock_data: Dict[str, Any]) -> bool:
        """Write lock_data via temp file + replace so readers never see a partial lock."""
        tmp_file = self.orchestra_dir / f".lock_{uuid.uuid4().hex[:6]}.tmp"
        try:
            with open(tmp_file, "w", encoding="utf-8") as f:
                json.dump(lock_data, f, indent=2)
            _retry_on_sharing_violation(lambda: tmp_file.replace(self.lock_file))
            return True
        except Exception:
            tmp_file.unlink(missing_ok=True)
            return False

    def acquire(
        self,
        task_id: str,
        agent_id: str,
        scope: str = "",
        lease_seconds: int = 1800,
        force: bool = False,
    ) -> bool:
        """
        Attempt to acquire the active writer lock.
        Returns True if acquired, False if held by another active agent.
        """
        with self._guard():
            now = time.time()
            current = self.get_current_lock()

            if current and not force:
                expires_at = current.get("expires_at", 0)
                holding_agent = current.get("agent_id")
                # If not expired and owned by someone else, reject
                if now < expires_at and holding_agent != agent_id:
                    return False

            return self._write_lock(
                {
                    "task_id": task_id,
                    "agent_id": agent_id,
                    "scope": scope,
                    "claimed_at": now,
                    "expires_at": now + lease_seconds,
                    "pid": os.getpid(),
                }
            )

    def release(self, agent_id: Optional[str] = None, force: bool = False) -> bool:
        """
        Release the active writer lock.
        If agent_id is provided, only releases if that agent owns it (unless force=True).
        """
        with self._guard():
            if not self.lock_file.is_file():
                return True

            current = self.get_current_lock()
            if current and not force and agent_id:
                if current.get("agent_id") != agent_id:
                    return False

            try:
                _retry_on_sharing_violation(lambda: self.lock_file.unlink(missing_ok=True))
                return True
            except Exception:
                return False

    def heartbeat(self, agent_id: str, extend_seconds: int = 1800) -> bool:
        """Extend lock lease for the holding agent."""
        with self._guard():
            current = self.get_current_lock()
            if not current or current.get("agent_id") != agent_id:
                return False

            current["expires_at"] = time.time() + extend_seconds
            return self._write_lock(current)

    def get_current_lock(self) -> Optional[Dict[str, Any]]:
        """Return the current lock dictionary, or None if unlocked or corrupt."""
        if not self.lock_file.is_file():
            return None
        try:
            with open(self.lock_file, "r", encoding="utf-8") as f:
                data = json.load(f)
                return data if isinstance(data, dict) else None
        except Exception:
            return None

    def is_locked(self) -> bool:
        """Check if an unexpired lock is currently held."""
        current = self.get_current_lock()
        if not current:
            return False
        return time.time() < current.get("expires_at", 0)


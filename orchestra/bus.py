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

from .models import AgentEvent, EventType


class EventBus:
    """
    Append-only file-based event stream backed by JSON Lines (.jsonl).
    Safe for concurrent multi-process access.
    """

    def __init__(self, orchestra_dir: Path | str, max_bytes: int = 5_000_000) -> None:
        self.orchestra_dir = Path(orchestra_dir).resolve()
        self.orchestra_dir.mkdir(parents=True, exist_ok=True)
        self.events_file = self.orchestra_dir / "events.jsonl"
        self.rotated_file = self.orchestra_dir / "events.1.jsonl"
        self.latest_event_file = self.orchestra_dir / "latest_event.json"
        self.max_bytes = max_bytes
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

        self._rotate_if_needed()

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

    def _rotate_if_needed(self) -> None:
        """Keep one previous generation once the log passes max_bytes."""
        try:
            if self.events_file.stat().st_size < self.max_bytes:
                return
            os.replace(self.events_file, self.rotated_file)
        except OSError:
            # Missing file, or another process has it open (Windows). Try again next emit.
            pass

    @staticmethod
    def _parse_lines(lines: List[str]) -> List[AgentEvent]:
        events: List[AgentEvent] = []
        for line in lines:
            line = line.strip()
            if not line:
                continue
            try:
                events.append(AgentEvent.from_json(line))
            except Exception:
                continue
        return events

    def current_offset(self) -> int:
        """Byte offset of the end of the log, for callers that only want future events."""
        try:
            return self.events_file.stat().st_size
        except OSError:
            return 0

    def read_from(self, offset: int = 0) -> Tuple[List[AgentEvent], int]:
        """
        Read events appended after byte `offset`. Returns (events, new_offset).
        Only complete lines are consumed, so a half-written event is picked up next call.
        If the log was rotated or truncated, reading restarts from the beginning.
        """
        try:
            size = self.events_file.stat().st_size
        except OSError:
            return [], 0
        if size < offset:
            offset = 0
        if size == offset:
            return [], offset

        try:
            with open(self.events_file, "rb") as f:
                f.seek(offset)
                chunk = f.read(size - offset)
        except OSError:
            return [], offset
        cut = chunk.rfind(b"\n")
        if cut == -1:
            return [], offset
        text = chunk[: cut + 1].decode("utf-8", errors="replace")
        return self._parse_lines(text.splitlines()), offset + cut + 1

    def _tail_lines(self, limit: int, block_size: int = 65536) -> List[str]:
        """Return the last `limit` lines without reading the whole file."""
        with open(self.events_file, "rb") as f:
            f.seek(0, os.SEEK_END)
            pos = f.tell()
            data = b""
            while pos > 0 and data.count(b"\n") <= limit:
                step = min(block_size, pos)
                pos -= step
                f.seek(pos)
                data = f.read(step) + data
        return data.decode("utf-8", errors="replace").splitlines()[-limit:]

    def read_events(
        self, since_timestamp: float = 0.0, limit: int = 100
    ) -> List[AgentEvent]:
        """Read the most recent `limit` events, optionally only those after a timestamp."""
        if not self.events_file.is_file() or limit <= 0:
            return []

        try:
            if since_timestamp <= 0:
                return self._parse_lines(self._tail_lines(limit))[-limit:]
            events, _ = self.read_from(0)
        except OSError:
            return []
        return [e for e in events if e.timestamp >= since_timestamp][-limit:]

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

    def stream_events(
        self, poll_interval: float = 0.5, start_from_beginning: bool = False
    ) -> Generator[AgentEvent, None, None]:
        """
        Continuously stream incoming events as a generator.
        Ideal for monitoring CLI commands or reactive daemons.
        """
        offset = 0 if start_from_beginning else self.current_offset()
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
    def _guard(self, timeout: float = 5.0, stale_after: float = 10.0) -> Iterator[None]:
        """
        Cross-process mutex around every read-then-write of the lock file.
        O_CREAT|O_EXCL is atomic on every OS, so only one process is ever inside.
        A guard left behind by a crashed process is removed after `stale_after` seconds.
        """
        deadline = time.time() + timeout
        while True:
            try:
                fd = os.open(self.guard_file, os.O_CREAT | os.O_EXCL | os.O_WRONLY)
                os.close(fd)
                break
            except (FileExistsError, PermissionError):
                try:
                    if time.time() - self.guard_file.stat().st_mtime > stale_after:
                        self.guard_file.unlink(missing_ok=True)
                        continue
                except OSError:
                    pass
                if time.time() > deadline:
                    raise TimeoutError("Could not take the lock guard")
                time.sleep(0.01)
        try:
            yield
        finally:
            try:
                self.guard_file.unlink(missing_ok=True)
            except OSError:
                pass

    def _write_lock(self, lock_data: Dict[str, Any]) -> bool:
        tmp_file = self.orchestra_dir / f".lock_{uuid.uuid4().hex[:6]}.tmp"
        try:
            with open(tmp_file, "w", encoding="utf-8") as f:
                json.dump(lock_data, f, indent=2)
            tmp_file.replace(self.lock_file)
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
        try:
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
        except TimeoutError:
            return False

    def release(self, agent_id: Optional[str] = None, force: bool = False) -> bool:
        """
        Release the active writer lock.
        If agent_id is provided, only releases if that agent owns it (unless force=True).
        """
        try:
            with self._guard():
                if not self.lock_file.is_file():
                    return True

                current = self.get_current_lock()
                if current and not force and agent_id:
                    if current.get("agent_id") != agent_id:
                        return False

                self.lock_file.unlink(missing_ok=True)
                return True
        except (TimeoutError, OSError):
            return False

    def heartbeat(self, agent_id: str, extend_seconds: int = 1800) -> bool:
        """Extend lock lease for the holding agent."""
        try:
            with self._guard():
                current = self.get_current_lock()
                if not current or current.get("agent_id") != agent_id:
                    return False
                current["expires_at"] = time.time() + extend_seconds
                return self._write_lock(current)
        except TimeoutError:
            return False

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

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
from pathlib import Path
from typing import Any, Callable, Dict, Generator, List, Optional

from .models import AgentEvent, EventType


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

    def stream_events(
        self, poll_interval: float = 0.5, start_from_beginning: bool = False
    ) -> Generator[AgentEvent, None, None]:
        """
        Continuously stream incoming events as a generator.
        Ideal for monitoring CLI commands or reactive daemons.
        """
        last_pos = 0
        if not start_from_beginning and self.events_file.is_file():
            last_pos = self.events_file.stat().st_size

        while True:
            if self.events_file.is_file():
                current_size = self.events_file.stat().st_size
                if current_size > last_pos:
                    with open(self.events_file, "r", encoding="utf-8") as f:
                        f.seek(last_pos)
                        for line in f:
                            line = line.strip()
                            if line:
                                try:
                                    yield AgentEvent.from_json(line)
                                except Exception:
                                    pass
                        last_pos = f.tell()
                elif current_size < last_pos:
                    # File was truncated/rotated
                    last_pos = 0
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
        now = time.time()
        current = self.get_current_lock()

        if current and not force:
            expires_at = current.get("expires_at", 0)
            holding_agent = current.get("agent_id")
            # If not expired and owned by someone else, reject
            if now < expires_at and holding_agent != agent_id:
                return False

        lock_data = {
            "task_id": task_id,
            "agent_id": agent_id,
            "scope": scope,
            "claimed_at": now,
            "expires_at": now + lease_seconds,
            "pid": os.getpid(),
        }

        tmp_file = self.orchestra_dir / f".lock_{uuid.uuid4().hex[:6]}.tmp"
        try:
            with open(tmp_file, "w", encoding="utf-8") as f:
                json.dump(lock_data, f, indent=2)
            tmp_file.replace(self.lock_file)
            return True
        except Exception:
            if tmp_file.is_file():
                tmp_file.unlink(missing_ok=True)
            return False

    def release(self, agent_id: Optional[str] = None, force: bool = False) -> bool:
        """
        Release the active writer lock.
        If agent_id is provided, only releases if that agent owns it (unless force=True).
        """
        if not self.lock_file.is_file():
            return True

        current = self.get_current_lock()
        if current and not force and agent_id:
            if current.get("agent_id") != agent_id:
                return False

        try:
            self.lock_file.unlink(missing_ok=True)
            return True
        except Exception:
            return False

    def heartbeat(self, agent_id: str, extend_seconds: int = 1800) -> bool:
        """Extend lock lease for the holding agent."""
        current = self.get_current_lock()
        if not current or current.get("agent_id") != agent_id:
            return False

        current["expires_at"] = time.time() + extend_seconds
        tmp_file = self.orchestra_dir / f".lock_hb_{uuid.uuid4().hex[:6]}.tmp"
        try:
            with open(tmp_file, "w", encoding="utf-8") as f:
                json.dump(current, f, indent=2)
            tmp_file.replace(self.lock_file)
            return True
        except Exception:
            if tmp_file.is_file():
                tmp_file.unlink(missing_ok=True)
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


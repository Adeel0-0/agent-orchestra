"""
Core data models and schemas for Agent Orchestra.
All structures support strict JSON serialization and deserialization.
"""

from __future__ import annotations
import json
import time
from dataclasses import dataclass, field, asdict
from enum import Enum
from typing import Any, Dict, List, Optional


class EventType(str, Enum):
    AGENT_CLAIM = "AGENT_CLAIM"
    AGENT_HEARTBEAT = "AGENT_HEARTBEAT"
    TASK_PROGRESS = "TASK_PROGRESS"
    HANDOFF_REQUEST = "HANDOFF_REQUEST"
    REVIEW_REQUEST = "REVIEW_REQUEST"
    REVIEW_VERDICT = "REVIEW_VERDICT"
    VERIFICATION_STARTED = "VERIFICATION_STARTED"
    VERIFICATION_RESULT = "VERIFICATION_RESULT"
    GIT_COMMITTED = "GIT_COMMITTED"
    GIT_SYNCED = "GIT_SYNCED"
    CI_UPDATE = "CI_UPDATE"
    EMERGENCY_STOP = "EMERGENCY_STOP"
    RESUME_WORK = "RESUME_WORK"
    CUSTOM_EVENT = "CUSTOM_EVENT"


class OrchestraState(str, Enum):
    IDLE_CLEAN = "IDLE_CLEAN"
    WRITER_ACTIVE = "WRITER_ACTIVE"
    READY_NEEDS_VERIFY = "READY_NEEDS_VERIFY"
    VERIFYING = "VERIFYING"
    READY_VERIFIED = "READY_VERIFIED"
    SYNCING = "SYNCING"
    CI_MONITORING = "CI_MONITORING"
    PAUSED_CHECKPOINT = "PAUSED_CHECKPOINT"
    ERROR = "ERROR"


@dataclass
class TaskClaim:
    task_id: str
    agent_id: str
    base_commit: str
    scope_description: str
    claimed_files: List[str] = field(default_factory=list)
    timestamp: float = field(default_factory=time.time)
    expires_at: Optional[float] = None

    def to_dict(self) -> Dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> TaskClaim:
        return cls(
            task_id=str(data.get("task_id", "")),
            agent_id=str(data.get("agent_id", "")),
            base_commit=str(data.get("base_commit", "")),
            scope_description=str(data.get("scope_description", "")),
            claimed_files=list(data.get("claimed_files", [])),
            timestamp=float(data.get("timestamp", time.time())),
            expires_at=data.get("expires_at"),
        )


@dataclass
class HandoffPayload:
    task_id: str
    agent_id: str
    status: str  # "complete" | "paused" | "failed"
    summary: str
    changed_files: List[str] = field(default_factory=list)
    test_results: Dict[str, Any] = field(default_factory=dict)
    timestamp: float = field(default_factory=time.time)

    def to_dict(self) -> Dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> HandoffPayload:
        return cls(
            task_id=str(data.get("task_id", "")),
            agent_id=str(data.get("agent_id", "")),
            status=str(data.get("status", "complete")),
            summary=str(data.get("summary", "")),
            changed_files=list(data.get("changed_files", [])),
            test_results=dict(data.get("test_results", {})),
            timestamp=float(data.get("timestamp", time.time())),
        )


@dataclass
class ReviewVerdict:
    task_id: str
    reviewer_agent_id: str
    accepted: bool
    comments: str
    findings: List[str] = field(default_factory=list)
    timestamp: float = field(default_factory=time.time)

    def to_dict(self) -> Dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> ReviewVerdict:
        return cls(
            task_id=str(data.get("task_id", "")),
            reviewer_agent_id=str(data.get("reviewer_agent_id", "")),
            accepted=bool(data.get("accepted", False)),
            comments=str(data.get("comments", "")),
            findings=list(data.get("findings", [])),
            timestamp=float(data.get("timestamp", time.time())),
        )


@dataclass
class AgentEvent:
    event_id: str
    event_type: EventType
    source_agent: str
    payload: Dict[str, Any]
    timestamp: float = field(default_factory=time.time)

    def to_dict(self) -> Dict[str, Any]:
        d = asdict(self)
        d["event_type"] = self.event_type.value
        return d

    def to_json(self) -> str:
        return json.dumps(self.to_dict(), indent=2)

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> AgentEvent:
        event_type_str = data.get("event_type", EventType.CUSTOM_EVENT.value)
        try:
            event_type = EventType(event_type_str)
        except ValueError:
            event_type = EventType.CUSTOM_EVENT

        return cls(
            event_id=str(data.get("event_id", "")),
            event_type=event_type,
            source_agent=str(data.get("source_agent", "unknown")),
            payload=dict(data.get("payload", {})),
            timestamp=float(data.get("timestamp", time.time())),
        )

    @classmethod
    def from_json(cls, raw: str) -> AgentEvent:
        return cls.from_dict(json.loads(raw))


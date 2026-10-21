"""
Agent Orchestra
===============
Autonomous Multi-Agent Coordination, File-Based IPC Event Bus, and Reactive Execution Engine.
Works with Claude Code, OpenAI Codex, Antigravity, Cursor, and custom scripts.
"""

__version__ = "0.1.0"
__author__ = "Syed Mohammad Adeel"

from .models import (
    AgentEvent,
    EventType,
    TaskClaim,
    HandoffPayload,
    ReviewVerdict,
    OrchestraState,
)
from .bus import EventBus, LockManager
from .config import OrchestraConfig, load_config
from .runner import ScriptRunner, ExecutionResult
from .daemon import OrchestraDaemon

__all__ = [
    "AgentEvent",
    "EventType",
    "TaskClaim",
    "HandoffPayload",
    "ReviewVerdict",
    "OrchestraState",
    "EventBus",
    "LockManager",
    "OrchestraConfig",
    "load_config",
    "ScriptRunner",
    "ExecutionResult",
    "OrchestraDaemon",
]


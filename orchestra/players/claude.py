"""
Claude Pro / Max through the official Claude Code CLI (`claude -p --output-format stream-json`).

Event shapes were recorded from Claude Code 2.1.319 (tests/fixtures/claude_tool.jsonl):
  system/init {session_id, model}
  assistant {message.content[]: text | thinking | tool_use}
  user {message.content[]: tool_result}
  result {is_error, result, stop_reason, total_cost_usd, usage, permission_denials}
"""

from __future__ import annotations

from typing import Any, Dict, List, Sequence, Tuple

from .base import (
    FILE_CHANGED, NOTICE, REASONING_DELTA, SESSION_STARTED, TOOL_FINISHED, TOOL_OUTPUT,
    TOOL_STARTED, USAGE, CliPlayer, Event, Mapper,
)

_PERMISSION = {"read-only": "plan", "edit": "acceptEdits", "full": "bypassPermissions"}
_EDIT_TOOLS = {"Edit", "Write", "MultiEdit", "NotebookEdit"}


def _result_text(content: Any) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "".join(c.get("text", "") for c in content if isinstance(c, dict))
    return ""


class ClaudeMapper(Mapper):
    def __init__(self, cwd) -> None:
        super().__init__(cwd)
        self._edits: Dict[str, str] = {}  # tool_use id -> path, for file.changed on success

    def feed(self, obj: Dict[str, Any]) -> List[Event]:
        kind = obj.get("type")

        if kind == "system" and obj.get("subtype") == "init":
            self.session_id = obj.get("session_id")
            return [Event(SESSION_STARTED, {"backend_session_id": self.session_id, "model": obj.get("model")})]

        if kind == "assistant":
            events: List[Event] = []
            for block in (obj.get("message") or {}).get("content") or []:
                btype = block.get("type")
                if btype == "text":
                    events += self.text(block.get("text", ""), new_block=True)
                elif btype == "thinking":
                    events.append(Event(REASONING_DELTA, {"text": block.get("thinking", "")}))
                elif btype == "tool_use":
                    tool_input = block.get("input") or {}
                    if block.get("name") in _EDIT_TOOLS:
                        path = tool_input.get("file_path") or tool_input.get("notebook_path")
                        if path:
                            self._edits[block.get("id", "")] = path
                    events.append(Event(TOOL_STARTED, {
                        "id": block.get("id", ""), "name": block.get("name", ""), "input": tool_input,
                    }))
            return events

        if kind == "user":
            events = []
            for block in (obj.get("message") or {}).get("content") or []:
                if not isinstance(block, dict) or block.get("type") != "tool_result":
                    continue
                tid = block.get("tool_use_id", "")
                ok = not block.get("is_error", False)
                output = _result_text(block.get("content"))
                if output:
                    events.append(Event(TOOL_OUTPUT, {"id": tid, "chunk": output}))
                if ok and tid in self._edits:
                    events.append(Event(FILE_CHANGED, {"path": self.rel_path(self._edits.pop(tid)), "kind": "update"}))
                events.append(Event(TOOL_FINISHED, {"id": tid, "ok": ok}))
            return events

        if kind == "result":
            u = obj.get("usage") or {}
            events = [Event(USAGE, {
                "input_tokens": u.get("input_tokens", 0)
                + u.get("cache_creation_input_tokens", 0)
                + u.get("cache_read_input_tokens", 0),
                "cached_input_tokens": u.get("cache_read_input_tokens", 0),
                "output_tokens": u.get("output_tokens", 0),
                # What the run would cost at API prices; a subscription login does not bill it.
                "cost_usd": obj.get("total_cost_usd"),
            })]
            for denial in obj.get("permission_denials") or []:
                events.append(Event(NOTICE, {"message": f"Permission denied: {denial.get('tool_name', 'tool')}"}))
            if obj.get("is_error"):
                events += self.error(str(obj.get("result") or obj.get("subtype") or "Claude reported an error"))
                return events + self.complete("error")
            return events + self.complete(obj.get("stop_reason") or "end_turn")

        return []


class ClaudePlayer(CliPlayer):
    id = "claude"
    display_name = "Claude Code"
    executable = "claude"
    install_hint = "npm install -g @anthropic-ai/claude-code"
    login_hint = "claude  (then /login)"

    def build_command(self, prompt: str) -> Tuple[Sequence[str], str]:
        argv = ["claude", "-p", "--output-format", "stream-json", "--verbose",
                "--permission-mode", _PERMISSION[self.permission]]
        if self.model:
            argv += ["--model", self.model]
        if self.session_id:
            argv += ["--resume", self.session_id]
        return argv, prompt

    def new_mapper(self) -> Mapper:
        return ClaudeMapper(self.cwd)

    @classmethod
    def login_check(cls) -> Sequence[str]:
        return ["claude", "auth", "status"]

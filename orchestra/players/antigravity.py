"""
Google AI Pro / Ultra through the official Antigravity CLI (`agy`, headless stream-json).

Event shapes recorded from agy 1.5.14 on 2026-11-21 (tests/fixtures/agy_*.jsonl):
  {"event":"init","conversation_id":...,"init":{"cwd","tools","permission_mode"}}
  {"event":"step_update","step_update":{"conversation_id","step_index",
      "state":"ACTIVE"|"DONE"|"ERROR",
      "step_type":"user_input"|"agent_response"|"tool"|"system_message"|...,
      "tool_name","text_delta","usage","tool_info":{"name","parameters","output"?,"error"?}}}
  {"event":"result","result":{"conversation_id","status":"SUCCESS"|"ERROR"|"CANCELED"|...,
      "response","error"?,"num_turns","usage":{...},"denied_actions"?:[{"action","display_name"}]}}

What the real runs showed, and the docs did not say:
- text_delta is always incremental, including on the DONE event (which carries the tail,
  often "\\n"). Every delta is appended; none is a repeat.
- A failed or denied tool ends with state "ERROR" and tool_info.error, not "DONE".
- usage.output_tokens already includes thinking_tokens; input_tokens excludes cache_read_tokens.
- Headless agy cannot ask for permission. A denied command ends the turn with an empty
  response and status SUCCESS, listing it in denied_actions.
- Modes: `--mode accept-edits` and `--mode plan` deny every shell command headlessly;
  `--mode auto` is not available on every account. The default mode with `--sandbox` runs
  commands under terminal restrictions and allows edits, which matches Codex's workspace-write.
"""

from __future__ import annotations

import json
from typing import Any, Dict, List, Optional, Sequence, Set, Tuple

from .base import (
    FILE_CHANGED, NOTICE, SESSION_STARTED, TOOL_FINISHED, TOOL_OUTPUT, TOOL_STARTED, USAGE,
    CliPlayer, Event, Mapper,
)

# Tool names from the init event's tool list plus edit_file and write_file seen in real runs.
_WRITE_TOOLS = {
    "edit_file", "write_file", "write_to_file", "replace_file_content", "multi_replace_file_content",
    "sed_file", "notebook_edit",
}
_PATH_KEYS = ("Path", "TargetFile", "AbsolutePath", "FilePath", "path", "file_path")
_FINISHED_STATES = {"DONE", "ERROR"}


def _path_from(params: Dict[str, Any]) -> Optional[str]:
    for key in _PATH_KEYS:
        if isinstance(params.get(key), str) and params[key]:
            return params[key]
    return None


class AntigravityMapper(Mapper):
    def __init__(self, cwd) -> None:
        super().__init__(cwd)
        self._tools_started: Set[int] = set()
        self._tools_done: Set[int] = set()
        self._text_steps: Set[int] = set()   # agent_response steps that have produced text

    def feed(self, obj: Dict[str, Any]) -> List[Event]:
        kind = obj.get("event")

        if kind == "init":
            self.session_id = obj.get("conversation_id") or None
            init = obj.get("init") or {}
            return [Event(SESSION_STARTED, {"backend_session_id": self.session_id, "model": init.get("model")})]

        if kind == "step_update":
            return self._step(obj.get("step_update") or {})

        if kind == "result":
            return self._result(obj.get("result") or {})

        return []

    def _result(self, res: Dict[str, Any]) -> List[Event]:
        u = res.get("usage") or {}
        cached = u.get("cache_read_tokens", 0)
        events = [Event(USAGE, {
            "input_tokens": u.get("input_tokens", 0) + cached,
            "cached_input_tokens": cached,
            "output_tokens": u.get("output_tokens", 0),   # already includes thinking tokens
            "cost_usd": None,                               # covered by the subscription
        })]
        if not self.session_id and res.get("conversation_id"):
            self.session_id = res["conversation_id"]

        denied = [d.get("display_name") or d.get("action") or "tool" for d in res.get("denied_actions") or []]
        for name in denied:
            events.append(Event(NOTICE, {"message": f"Permission denied: {name}"}))

        status = str(res.get("status", "SUCCESS")).upper()
        if status == "SUCCESS":
            if not self._text_started:
                response = res.get("response", "")
                if response:
                    events += self.text(response)
                elif denied:
                    # Headless agy stops the turn when it is refused; say why instead of an empty reply.
                    events.append(Event(NOTICE, {"message": (
                        "Antigravity stopped because it needed permission it cannot ask for in this mode. "
                        "Use --permission edit to let it run commands in its sandbox, or full for no limits.")}))
            return events + self.complete("end_turn")
        if status in ("CANCELED", "CANCELLED", "INTERRUPTED"):
            return events + self.complete("cancelled")
        events += self.error(res.get("error") or f"Antigravity run ended with status {status}")
        return events + self.complete("error")

    def _step(self, step: Dict[str, Any]) -> List[Event]:
        stype = step.get("step_type")
        index = int(step.get("step_index", -1))
        finished = step.get("state") in _FINISHED_STATES

        if stype == "agent_response":
            delta = step.get("text_delta") or ""
            if not delta:
                return []
            first_delta_of_step = index not in self._text_steps
            self._text_steps.add(index)
            return self.text(delta, new_block=first_delta_of_step)

        if stype == "tool":
            return self._tool(step, index, finished)

        return []

    def _tool(self, step: Dict[str, Any], index: int, finished: bool) -> List[Event]:
        info = step.get("tool_info") or {}
        name = step.get("tool_name") or info.get("name") or "tool"
        params = info.get("parameters") or {}
        tid = str(index)
        events: List[Event] = []
        if index not in self._tools_started:
            self._tools_started.add(index)
            events.append(Event(TOOL_STARTED, {"id": tid, "name": name, "input": params}))
        if finished and index not in self._tools_done:
            self._tools_done.add(index)
            error = info.get("error")
            ok = step.get("state") == "DONE" and not error
            if info.get("output"):
                events.append(Event(TOOL_OUTPUT, {"id": tid, "chunk": str(info["output"])}))
            elif isinstance(error, dict) and error.get("message"):
                events.append(Event(TOOL_OUTPUT, {"id": tid, "chunk": str(error["message"])}))
            path = _path_from(params)
            if ok and name in _WRITE_TOOLS and path:
                events.append(Event(FILE_CHANGED, {"path": self.rel_path(path), "kind": "update"}))
            events.append(Event(TOOL_FINISHED, {"id": tid, "ok": ok}))
        return events


class AntigravityPlayer(CliPlayer):
    id = "antigravity"
    display_name = "Antigravity (Google AI Pro)"
    executable = "agy"
    install_hint = "irm https://antigravity.google/cli/install.ps1 | iex   (PowerShell)"
    login_hint = "agy   (sign in once with the Google account that has AI Pro)"

    def build_command(self, prompt: str) -> Tuple[Sequence[str], str]:
        # The prompt goes on stdin as one stream-json message: no command-line length limit,
        # and nothing in the prompt can be read as a flag. Closing stdin ends the session
        # after this one turn.
        argv = ["agy", "--input-format", "stream-json", "--output-format", "stream-json"]
        if self.permission == "read-only":
            argv += ["--mode", "plan"]
        elif self.permission == "edit":
            argv += ["--sandbox"]           # default mode + sandbox: edits and sandboxed commands
        else:
            argv += ["--dangerously-skip-permissions"]
        if self.model:
            argv += ["--model", self.model]
        if self.session_id:
            argv += ["--conversation", self.session_id]
        message = {"event": "user", "message": {"content": prompt}}
        return argv, json.dumps(message, ensure_ascii=False) + "\n"

    def new_mapper(self) -> Mapper:
        return AntigravityMapper(self.cwd)

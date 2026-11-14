"""
Google AI Pro / Ultra through the official Antigravity CLI (`agy -p ... --output-format stream-json`).

NOT YET RECORDED FROM A REAL RUN: agy is not installed on the development machine.
The shapes below come from https://antigravity.google/docs/cli/headless/ (read 2026-11-14):
  {"event":"init","conversation_id":...,"init":{"cwd","tools","permission_mode","model"?}}
  {"event":"step_update","step_update":{"conversation_id","step_index","state":"ACTIVE"|"DONE",
      "step_type":"user_input"|"agent_response"|"tool"|"checkpoint","tool_name","text_delta",
      "usage","tool_info":{"name","parameters","output","error"?}}}
  {"event":"result","result":{"conversation_id","status":"SUCCESS"|"ERROR"|"CANCELED"|...,
      "response","error"?,"num_turns","usage":{input_tokens,output_tokens,thinking_tokens,...}}}
Undocumented, so guessed: whether a DONE agent_response repeats the text, and which
parameter of a write tool holds the path. Record tests/fixtures/agy_*.jsonl on first install.
"""

from __future__ import annotations

from typing import Any, Dict, List, Optional, Sequence, Set, Tuple

from .base import (
    FILE_CHANGED, SESSION_STARTED, TOOL_FINISHED, TOOL_OUTPUT, TOOL_STARTED, USAGE,
    CliPlayer, Event, Mapper,
)

_WRITE_TOOLS = {"write_to_file", "replace_file_content", "edit_file", "multi_replace_file_content"}
_PATH_KEYS = ("TargetFile", "AbsolutePath", "FilePath", "path", "file_path")


def _path_from(params: Dict[str, Any]) -> Optional[str]:
    for key in _PATH_KEYS:
        if isinstance(params.get(key), str):
            return params[key]
    return None


class AntigravityMapper(Mapper):
    def __init__(self, cwd) -> None:
        super().__init__(cwd)
        self._tools_started: Set[int] = set()
        self._tools_done: Set[int] = set()
        self._text_steps: Set[int] = set()   # agent_response steps that already streamed text

    def feed(self, obj: Dict[str, Any]) -> List[Event]:
        kind = obj.get("event")

        if kind == "init":
            self.session_id = obj.get("conversation_id") or None
            init = obj.get("init") or {}
            return [Event(SESSION_STARTED, {"backend_session_id": self.session_id, "model": init.get("model")})]

        if kind == "step_update":
            return self._step(obj.get("step_update") or {})

        if kind == "result":
            res = obj.get("result") or {}
            u = res.get("usage") or {}
            events = [Event(USAGE, {
                "input_tokens": u.get("input_tokens", 0),
                "cached_input_tokens": u.get("cache_read_tokens", 0),
                "output_tokens": u.get("output_tokens", 0) + u.get("thinking_tokens", 0),
                "cost_usd": None,
            })]
            if not self.session_id and res.get("conversation_id"):
                self.session_id = res["conversation_id"]
            status = str(res.get("status", "SUCCESS")).upper()
            if status == "SUCCESS":
                # If nothing streamed (older CLI, or text only in the envelope), show the response.
                if not self._text_started:
                    events += self.text(res.get("response", ""))
                return events + self.complete("end_turn")
            if status in ("CANCELED", "INTERRUPTED"):
                return events + self.complete("cancelled")
            events += self.error(res.get("error") or f"Antigravity run ended with status {status}")
            return events + self.complete("error")

        return []

    def _step(self, step: Dict[str, Any]) -> List[Event]:
        stype = step.get("step_type")
        index = int(step.get("step_index", -1))
        done = step.get("state") == "DONE"

        if stype == "agent_response":
            delta = step.get("text_delta") or ""
            if not done and delta:
                first_delta_of_step = index not in self._text_steps
                self._text_steps.add(index)
                return self.text(delta, new_block=first_delta_of_step)
            # Only use a DONE event's text when this step streamed none, so it is never doubled.
            if done and delta and index not in self._text_steps:
                self._text_steps.add(index)
                return self.text(delta, new_block=True)
            return []

        if stype == "tool":
            info = step.get("tool_info") or {}
            name = step.get("tool_name") or info.get("name") or "tool"
            params = info.get("parameters") or {}
            tid = str(index)
            events: List[Event] = []
            if index not in self._tools_started:
                self._tools_started.add(index)
                events.append(Event(TOOL_STARTED, {"id": tid, "name": name, "input": params}))
            if done and index not in self._tools_done:
                self._tools_done.add(index)
                ok = not info.get("error")
                if info.get("output"):
                    events.append(Event(TOOL_OUTPUT, {"id": tid, "chunk": str(info["output"])}))
                elif info.get("error"):
                    events.append(Event(TOOL_OUTPUT, {"id": tid, "chunk": str(info["error"].get("message", ""))}))
                path = _path_from(params)
                if ok and name in _WRITE_TOOLS and path:
                    events.append(Event(FILE_CHANGED, {"path": self.rel_path(path), "kind": "update"}))
                events.append(Event(TOOL_FINISHED, {"id": tid, "ok": ok}))
            return events

        return []


class AntigravityPlayer(CliPlayer):
    id = "antigravity"
    display_name = "Antigravity (Google AI Pro)"
    executable = "agy"
    install_hint = "irm https://antigravity.google/cli/install.ps1 | iex   (PowerShell)"
    login_hint = "agy   (sign in once with the Google account that has AI Pro)"

    def build_command(self, prompt: str) -> Tuple[Sequence[str], Optional[str]]:
        # The documented headless form takes the prompt as the -p value. agy is a native .exe,
        # so the argument is not reinterpreted by cmd.exe.
        argv = ["agy", "-p", prompt, "--output-format", "stream-json"]
        if self.permission == "read-only":
            argv += ["--mode", "plan"]
        elif self.permission == "edit":
            argv += ["--mode", "accept-edits"]
        else:
            argv += ["--dangerously-skip-permissions"]
        if self.model:
            argv += ["--model", self.model]
        if self.session_id:
            argv += ["--conversation", self.session_id]
        return argv, None

    def new_mapper(self) -> Mapper:
        return AntigravityMapper(self.cwd)

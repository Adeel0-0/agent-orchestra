"""
ChatGPT Plus / Pro through the official Codex CLI (`codex exec --json`).

Event shapes were recorded from codex-cli 0.166.0 (tests/fixtures/codex_*.jsonl):
  thread.started {thread_id}            turn.started
  item.started / item.completed {item}  item.type: agent_message, reasoning,
                                        command_execution, file_change, error, ...
  turn.completed {usage}                error {message}   turn.failed {error}
"""

from __future__ import annotations

import re
from typing import Any, Dict, List, Sequence, Tuple

from .base import (
    FILE_CHANGED, NOTICE, REASONING_DELTA, SESSION_STARTED, TOOL_FINISHED, TOOL_OUTPUT,
    TOOL_STARTED, USAGE, CliPlayer, Event, Mapper,
)

_SANDBOX = {"read-only": "read-only", "edit": "workspace-write", "full": "danger-full-access"}

# Codex runs every command through a shell wrapper; show the command the model actually wrote.
_WRAPPER = re.compile(
    r"""^\s*"?[^"]*?(?:powershell|pwsh)(?:\.exe)?"?\s+(?:-NoProfile\s+)?-Command\s+(?P<q>['"])(?P<cmd>.*)(?P=q)\s*$"""
    r"""|^\s*"?[^"]*?(?:bash|sh|zsh)"?\s+-l?c\s+(?P<q2>['"])(?P<cmd2>.*)(?P=q2)\s*$""",
    re.IGNORECASE | re.DOTALL,
)


def display_command(command: str) -> str:
    m = _WRAPPER.match(command or "")
    if not m:
        return command
    inner = m.group("cmd") if m.group("cmd") is not None else m.group("cmd2")
    quote = m.group("q") or m.group("q2")
    # PowerShell doubles a quote to escape it inside a same-quoted string.
    return inner.replace(quote * 2, quote) if quote == "'" else inner.replace('\\"', '"')


class CodexMapper(Mapper):
    def feed(self, obj: Dict[str, Any]) -> List[Event]:
        kind = obj.get("type")

        if kind == "thread.started":
            self.session_id = obj.get("thread_id")
            return [Event(SESSION_STARTED, {"backend_session_id": self.session_id, "model": None})]

        if kind in ("item.started", "item.completed"):
            return self._item(obj.get("item") or {}, done=kind == "item.completed")

        if kind == "turn.completed":
            u = obj.get("usage") or {}
            return [
                Event(USAGE, {
                    "input_tokens": u.get("input_tokens", 0),
                    "cached_input_tokens": u.get("cached_input_tokens", 0),
                    "output_tokens": u.get("output_tokens", 0),
                    "cost_usd": None,  # covered by the subscription
                }),
                *self.complete("end_turn"),
            ]

        if kind == "error":
            return self.error(str(obj.get("message", "Codex reported an error")))

        if kind == "turn.failed":
            events = [] if self.errored else self.error(str((obj.get("error") or {}).get("message", "Turn failed")))
            return events + self.complete("error")

        return []

    def _item(self, item: Dict[str, Any], done: bool) -> List[Event]:
        itype = item.get("type")
        iid = str(item.get("id", ""))

        if itype == "agent_message":
            return self.text(item.get("text", ""), new_block=True) if done else []

        if itype == "reasoning":
            return [Event(REASONING_DELTA, {"text": item.get("text", "")})] if done else []

        if itype == "command_execution":
            if not done:
                raw = item.get("command", "")
                return [Event(TOOL_STARTED, {"id": iid, "name": "shell",
                                             "input": {"command": display_command(raw), "raw_command": raw}})]
            events = []
            if item.get("aggregated_output"):
                events.append(Event(TOOL_OUTPUT, {"id": iid, "chunk": item["aggregated_output"]}))
            events.append(Event(TOOL_FINISHED, {"id": iid, "ok": item.get("exit_code") == 0}))
            return events

        if itype == "file_change":
            changes = item.get("changes") or []
            if not done:
                paths = [self.rel_path(c.get("path", "")) for c in changes]
                return [Event(TOOL_STARTED, {"id": iid, "name": "edit", "input": {"paths": paths}})]
            ok = item.get("status") == "completed"
            events = []
            if ok:
                events = [
                    Event(FILE_CHANGED, {"path": self.rel_path(c.get("path", "")), "kind": c.get("kind", "update")})
                    for c in changes
                ]
            return events + [Event(TOOL_FINISHED, {"id": iid, "ok": ok})]

        if itype == "error":
            # Item-level errors are warnings (e.g. "Model metadata ... not found"); the turn goes on.
            return [Event(NOTICE, {"message": item.get("message", "")})] if done else []

        # mcp_tool_call, web_search, todo_list and anything newer: show it generically.
        if not done:
            return [Event(TOOL_STARTED, {"id": iid, "name": itype or "tool", "input": {}})]
        return [Event(TOOL_FINISHED, {"id": iid, "ok": item.get("status") != "failed"})]


class CodexPlayer(CliPlayer):
    id = "codex"
    display_name = "Codex (ChatGPT)"
    executable = "codex"
    install_hint = "npm install -g @openai/codex"
    login_hint = "codex login"

    def build_command(self, prompt: str) -> Tuple[Sequence[str], str]:
        common = ["--json", "--skip-git-repo-check"]
        if self.model:
            common += ["-m", self.model]
        if self.session_id:
            # `exec resume` has no -s or -C: the sandbox goes through config, the folder is our cwd.
            argv = ["codex", "exec", "resume", *common, "-c", f"sandbox_mode={_SANDBOX[self.permission]}",
                    self.session_id, "-"]
        else:
            argv = ["codex", "exec", *common, "-s", _SANDBOX[self.permission], "-"]
        return argv, prompt

    def new_mapper(self) -> Mapper:
        return CodexMapper(self.cwd)

    @classmethod
    def login_check(cls) -> Sequence[str]:
        return ["codex", "login", "status"]

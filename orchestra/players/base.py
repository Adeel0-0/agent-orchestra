"""
The contract every player follows.

A player wraps one agent backend. Whatever the backend speaks, the player yields the
same small set of events, so the terminal chat, the future UI and the transcript store
never need to know which model is behind a session.

CLI players are split in two:
- a Mapper turns one parsed JSON line from the CLI into zero or more Events. It holds no
  I/O, so it is tested directly against recorded fixtures.
- CliPlayer runs the CLI through ChildProcess and feeds every line to a fresh Mapper.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, AsyncIterator, Dict, List, Optional, Sequence, Tuple

from .process import ChildProcess, ProcessError

# --------------------------------------------------------------------------- events

SESSION_STARTED = "session.started"   # backend_session_id, model
TEXT_DELTA = "text.delta"             # text
REASONING_DELTA = "reasoning.delta"   # text
TOOL_STARTED = "tool.started"         # id, name, input
TOOL_OUTPUT = "tool.output"           # id, chunk
TOOL_FINISHED = "tool.finished"       # id, ok
FILE_CHANGED = "file.changed"         # path, kind
USAGE = "usage"                       # input_tokens, output_tokens, cost_usd
NOTICE = "notice"                     # message (warnings that do not stop the turn)
ERROR = "error"                       # message, kind, retryable
TURN_COMPLETED = "turn.completed"     # stop_reason: end_turn | error | cancelled

PERMISSION_MODES = ("read-only", "edit", "full")


@dataclass
class Event:
    type: str
    data: Dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> Dict[str, Any]:
        return {"type": self.type, **self.data}


def classify_error(message: str) -> Tuple[str, bool]:
    """
    Sort a backend error into a kind the UI and the router can act on.

    Returns (kind, retryable). Kinds the rest of Orchestra understands:
      "auth"   - not logged in or login expired. Show the login command. Not retryable.
      "quota"  - subscription or rate limit reached. Retryable later, or on another player.
      "model"  - unknown or unsupported model. Not retryable with the same settings.
      "other"  - anything else.

    TODO(owner): decide how messages map to kinds. This drives what the user is told and,
    in Phase 6, when Orchestra falls over to a different player. Real messages seen so far:
      Codex:  "The 'no-such-model-xyz' model is not supported when using Codex with a ChatGPT account."
      agy:    "authentication required"  (documented), "invalid model selection ..." (documented)
    """
    return "other", False


def error_event(message: str) -> Event:
    kind, retryable = classify_error(message)
    return Event(ERROR, {"message": message, "kind": kind, "retryable": retryable})


# --------------------------------------------------------------------------- mapper


class Mapper:
    """Turns a CLI's JSON lines into Events. Subclasses implement `feed`."""

    def __init__(self, cwd: Path | str) -> None:
        self.cwd = Path(cwd)
        self.session_id: Optional[str] = None
        self.completed = False
        self.errored = False
        self._text_started = False

    def feed(self, obj: Dict[str, Any]) -> List[Event]:
        raise NotImplementedError

    # helpers for subclasses -------------------------------------------------

    def text(self, text: str, new_block: bool = False) -> List[Event]:
        """Assistant text. new_block=True separates whole messages with a blank line."""
        if not text:
            return []
        if new_block and self._text_started:
            text = "\n\n" + text
        self._text_started = True
        return [Event(TEXT_DELTA, {"text": text})]

    def error(self, message: str) -> List[Event]:
        self.errored = True
        return [error_event(message)]

    def complete(self, stop_reason: str = "end_turn") -> List[Event]:
        if self.completed:
            return []
        self.completed = True
        return [Event(TURN_COMPLETED, {"stop_reason": stop_reason})]

    def rel_path(self, path: str) -> str:
        """Show paths inside the project relative to it."""
        try:
            return str(Path(path).resolve().relative_to(self.cwd.resolve()))
        except (ValueError, OSError):
            return path

    def finish(self, exit_code: int, stderr: str) -> List[Event]:
        """Called once the CLI exits. Guarantees every turn ends with turn.completed."""
        events: List[Event] = []
        if exit_code != 0 and not self.errored:
            tail = "\n".join(stderr.strip().splitlines()[-5:]) or f"exited with code {exit_code}"
            events += self.error(tail)
        if not self.completed:
            events += self.complete("error" if (exit_code != 0 or self.errored) else "end_turn")
        return events


# --------------------------------------------------------------------------- player


class CliPlayer:
    """A player whose agent is an installed CLI. Subclasses fill in the class attributes."""

    id: str = ""
    display_name: str = ""
    executable: str = ""
    install_hint: str = ""
    login_hint: str = ""

    def __init__(
        self,
        cwd: Path | str,
        model: Optional[str] = None,
        permission: str = "edit",
        session_id: Optional[str] = None,
    ) -> None:
        if permission not in PERMISSION_MODES:
            raise ValueError(f"permission must be one of {PERMISSION_MODES}")
        self.cwd = Path(cwd).resolve()
        self.model = model
        self.permission = permission
        self.session_id = session_id
        self._child: Optional[ChildProcess] = None

    # subclasses -------------------------------------------------------------

    def build_command(self, prompt: str) -> Tuple[Sequence[str], str]:
        """Return (argv, stdin_text) for one turn. The prompt always goes on stdin."""
        raise NotImplementedError

    def new_mapper(self) -> Mapper:
        raise NotImplementedError

    @classmethod
    def login_check(cls) -> Optional[Sequence[str]]:
        """Command whose exit code 0 means logged in, or None if the CLI has none."""
        return None

    # running ----------------------------------------------------------------

    async def send(self, prompt: str) -> AsyncIterator[Event]:
        """Run one turn and yield its events. Cancelling the iteration kills the CLI."""
        argv, stdin_text = self.build_command(prompt)
        mapper = self.new_mapper()
        child = ChildProcess(argv, cwd=self.cwd, stdin_text=stdin_text)
        self._child = child
        try:
            try:
                await child.start()
            except ProcessError as exc:
                for evt in mapper.error(str(exc)) + mapper.complete("error"):
                    yield evt
                return

            async for line in child.lines():
                try:
                    obj = json.loads(line)
                except ValueError:
                    continue  # CLIs occasionally print plain text; it is not an event
                if not isinstance(obj, dict):
                    continue
                for evt in mapper.feed(obj):
                    if evt.type == SESSION_STARTED and evt.data.get("backend_session_id"):
                        self.session_id = evt.data["backend_session_id"]
                    yield evt

            code = await child.wait()
            for evt in mapper.finish(code, child.stderr):
                yield evt
        finally:
            # Runs on normal exit, on error, and when the caller stops iterating (Ctrl+C).
            await child.kill()
            self._child = None

    async def cancel(self) -> None:
        if self._child:
            await self._child.kill()

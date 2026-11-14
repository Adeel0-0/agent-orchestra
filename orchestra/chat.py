"""
`orchestra chat`: talk to any player from the terminal.

One prompt on the command line runs one turn and exits. With no prompt it opens a
conversation that keeps the same backend session until you type /exit.
Ctrl+C during a turn stops that turn (and kills the CLI); Ctrl+C at the prompt exits.
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
from pathlib import Path
from typing import Dict, Optional, TextIO

from .players import base
from .players.base import Player, Event
from .players.registry import check, get_player


class Renderer:
    """Prints events for a human. Keeps nothing but what it needs to lay out lines."""

    def __init__(self, out: TextIO = sys.stdout, color: Optional[bool] = None, login_hint: str = "") -> None:
        self.out = out
        if color is None:
            color = out.isatty() and not os.environ.get("NO_COLOR")
        if color and os.name == "nt":
            os.system("")  # turns on ANSI escape handling in older Windows consoles
        self.color = color
        self.login_hint = login_hint
        self.mid_line = False
        self.usage: Dict[str, object] = {}

    def _c(self, code: str, text: str) -> str:
        return f"\033[{code}m{text}\033[0m" if self.color else text

    def _line(self, text: str) -> None:
        if self.mid_line:
            self.out.write("\n")
            self.mid_line = False
        self.out.write(text + "\n")
        self.out.flush()

    @staticmethod
    def _summary(data: Dict[str, object]) -> str:
        inp = data.get("input") or {}
        if isinstance(inp, dict):
            for key in ("command", "CommandLine", "file_path", "path", "pattern", "url"):
                if inp.get(key):
                    return str(inp[key])
            if inp.get("paths"):
                return ", ".join(map(str, inp["paths"]))
            text = json.dumps(inp, ensure_ascii=False)
            return "" if text == "{}" else text
        return str(inp)

    def show(self, evt: Event) -> None:
        d = evt.data
        if evt.type == base.TEXT_DELTA:
            self.out.write(d["text"])
            self.out.flush()
            self.mid_line = not d["text"].endswith("\n")
        elif evt.type == base.TOOL_STARTED:
            summary = self._summary(d)
            if len(summary) > 100:
                summary = summary[:97] + "..."
            self._line(self._c("36", f"  > {d.get('name')}") + (f" {self._c('2', summary)}" if summary else ""))
        elif evt.type == base.TOOL_OUTPUT:
            lines = str(d.get("chunk", "")).rstrip().splitlines()
            shown = lines[-4:]
            if len(lines) > len(shown):
                self._line(self._c("2", f"    | ... {len(lines) - len(shown)} more lines"))
            for line in shown:
                self._line(self._c("2", f"    | {line[:160]}"))
        elif evt.type == base.TOOL_FINISHED:
            if not d.get("ok"):
                self._line(self._c("31", "    x failed"))
        elif evt.type == base.FILE_CHANGED:
            self._line(self._c("33", f"  ~ {d.get('path')} ({d.get('kind')})"))
        elif evt.type == base.NOTICE:
            self._line(self._c("33", f"  ! {d.get('message')}"))
        elif evt.type == base.PERMISSION_REQUESTED:
            self._line(f"  ? {d.get('tool')}: {json.dumps(d.get('input'), ensure_ascii=False)}")
        elif evt.type == base.ERROR:
            self._line(self._c("31", f"error: {d.get('message')}"))
            if d.get("kind") == "auth" and self.login_hint:
                self._line(f"  fix: {self.login_hint}")
        elif evt.type == base.USAGE:
            self.usage = d
        elif evt.type == base.TURN_COMPLETED:
            parts = []
            if self.usage:
                parts.append(f"{self.usage.get('input_tokens', 0)} in / {self.usage.get('output_tokens', 0)} out tokens")
                if self.usage.get('cost_usd') is not None:
                    parts.append(f"${self.usage['cost_usd']:.6f}")
            if d.get("stop_reason") not in (None, "end_turn"):
                parts.append(str(d["stop_reason"]))
            self._line(self._c("2", f"[{', '.join(parts)}]") if parts else "")
            self.usage = {}


def ask_permission() -> bool:
    """Non-interactive input always denies. Only an explicit yes grants an action."""
    if not sys.stdin.isatty():
        return False
    try:
        return input('Allow this action? [y/N] ').strip().lower() in {'y', 'yes'}
    except EOFError:
        return False


async def run_turn(player: Player, prompt: str, renderer: Optional[Renderer], as_json: bool) -> bool:
    """Run one turn. Returns True when it completed without an error event."""
    ok = True
    stream = player.send(prompt)
    try:
        async for evt in stream:
            if evt.type == base.ERROR:
                ok = False
            if as_json:
                print(json.dumps(evt.to_dict(), ensure_ascii=False), flush=True)
            elif renderer:
                renderer.show(evt)
            if evt.type == base.PERMISSION_REQUESTED:
                await player.answer_permission(evt.data['request_id'], False if as_json else ask_permission())
    finally:
        await stream.aclose()
    return ok


def _turn(player: Player, prompt: str, renderer: Optional[Renderer], as_json: bool) -> Optional[bool]:
    """Run a turn in its own event loop. Returns None if the user pressed Ctrl+C."""
    try:
        return asyncio.run(run_turn(player, prompt, renderer, as_json))
    except KeyboardInterrupt:
        # asyncio.run cancelled the turn, so CliPlayer.send's finally block killed the CLI.
        if renderer:
            renderer._line(renderer._c("2", "[stopped]"))
        return None


def chat(
    agent: str,
    prompt: Optional[str],
    cwd: Path,
    model: Optional[str] = None,
    permission: str = "edit",
    resume: Optional[str] = None,
    as_json: bool = False,
) -> int:
    player_cls = get_player(agent)
    # Validate model, resume and project before health checks, with actionable errors.
    player = player_cls(cwd=cwd, model=model, permission=permission, session_id=resume)
    health = check(player_cls)
    if not health.ready:
        print(f"{player_cls.display_name} is not ready: {'not installed' if not health.path else 'not logged in'}.",
              file=sys.stderr)
        print(f"  fix: {health.fix}", file=sys.stderr)
        return 2

    renderer = None if as_json else Renderer(login_hint=player_cls.login_hint)

    if prompt is not None:
        ok = _turn(player, prompt, renderer, as_json)
        if player.session_id and not as_json:
            print(f"session: {player.session_id}  (continue with --resume {player.session_id})", file=sys.stderr)
        return 0 if ok else (130 if ok is None else 1)

    print(f"{player_cls.display_name} in {cwd}  [{permission}]  /exit to quit")
    while True:
        try:
            line = input("\n> ").strip()
        except (EOFError, KeyboardInterrupt):
            print()
            return 0
        if not line:
            continue
        if line in ("/exit", "/quit"):
            return 0
        if line == "/session":
            print(player.session_id or "(none yet)")
            continue
        _turn(player, line, renderer, as_json)

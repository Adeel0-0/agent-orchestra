"""Which players exist, and whether each one is installed and logged in on this machine."""

from __future__ import annotations

import json
import subprocess
from dataclasses import dataclass
from typing import Dict, List, Optional, Type

from .antigravity import AntigravityPlayer
from .base import CliPlayer
from .claude import ClaudePlayer
from .codex import CodexPlayer
from .process import resolve_executable

PLAYERS: Dict[str, Type[CliPlayer]] = {
    p.id: p for p in (CodexPlayer, AntigravityPlayer, ClaudePlayer)
}


def get_player(player_id: str) -> Type[CliPlayer]:
    try:
        return PLAYERS[player_id]
    except KeyError:
        raise KeyError(f"Unknown player '{player_id}'. Known: {', '.join(PLAYERS)}") from None


@dataclass
class PlayerHealth:
    id: str
    name: str
    path: Optional[str]
    version: Optional[str]
    logged_in: Optional[bool]   # None = this CLI has no way to check
    fix: Optional[str]          # the command that would fix the first problem found

    @property
    def ready(self) -> bool:
        return self.path is not None and self.logged_in is not False


def _run(argv: List[str], timeout: int = 30) -> Optional[subprocess.CompletedProcess]:
    try:
        return subprocess.run(
            argv, capture_output=True, text=True, encoding="utf-8", errors="replace",
            stdin=subprocess.DEVNULL, timeout=timeout,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None


def _logged_in(res: Optional[subprocess.CompletedProcess]) -> bool:
    if res is None or res.returncode != 0:
        return False
    try:  # `claude auth status` prints JSON with loggedIn
        data = json.loads(res.stdout)
        if isinstance(data, dict) and "loggedIn" in data:
            return bool(data["loggedIn"])
    except ValueError:
        pass
    return "not logged in" not in (res.stdout + res.stderr).lower()


def check(player: Type[CliPlayer]) -> PlayerHealth:
    path = resolve_executable(player.executable)
    if path is None:
        return PlayerHealth(player.id, player.display_name, None, None, None, player.install_hint)

    res = _run([path, "--version"])
    version = (res.stdout or res.stderr).strip().splitlines()[0] if res and (res.stdout or res.stderr) else None

    check_cmd = player.login_check()
    if check_cmd is None:
        return PlayerHealth(player.id, player.display_name, path, version, None, None)
    logged_in = _logged_in(_run([path, *check_cmd[1:]]))
    return PlayerHealth(player.id, player.display_name, path, version, logged_in,
                        None if logged_in else player.login_hint)


def check_all() -> List[PlayerHealth]:
    return [check(p) for p in PLAYERS.values()]

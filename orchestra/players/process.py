"""
Async child-process runner for agent CLIs.

Every player starts its CLI through this module, so the Windows quirks live in one place:
- npm installs CLIs as `.cmd` shims, which Windows runs through cmd.exe. cmd.exe treats
  characters like & | % as syntax, so prompts are always sent on stdin, never as arguments,
  and arguments to a shim are refused if they contain those characters.
- Killing a shim kills only cmd.exe and orphans the real CLI, so cancel kills the whole tree.
"""

from __future__ import annotations

import asyncio
import os
import shutil
import signal
import subprocess
import sys
from pathlib import Path
from typing import AsyncIterator, Dict, List, Optional, Sequence

IS_WINDOWS = os.name == "nt"
_CMD_UNSAFE = set('&|<>^%"!\r\n')


class ProcessError(RuntimeError):
    pass


def resolve_executable(name: str) -> Optional[str]:
    """Full path of an executable on PATH, or None."""
    return shutil.which(name)


def _check_args(executable: str, args: Sequence[str]) -> None:
    if not executable.lower().endswith((".cmd", ".bat")):
        return
    for arg in args:
        if _CMD_UNSAFE & set(arg):
            raise ProcessError(
                f"Refusing to pass {arg!r} to {Path(executable).name}: cmd.exe would interpret it. "
                "Send text like this on stdin instead."
            )


class ChildProcess:
    """One running CLI. Read stdout line by line, then wait for the exit code."""

    def __init__(
        self,
        argv: Sequence[str],
        cwd: Path | str,
        stdin_text: Optional[str] = None,
        env: Optional[Dict[str, str]] = None,
    ) -> None:
        if not argv:
            raise ProcessError("Empty command")
        executable = resolve_executable(argv[0]) or argv[0]
        _check_args(executable, argv[1:])
        self.argv: List[str] = [executable, *argv[1:]]
        self.cwd = Path(cwd)
        self.stdin_text = stdin_text
        self.env = {**os.environ, **(env or {})}
        self.proc: Optional[asyncio.subprocess.Process] = None
        self._stderr_chunks: List[str] = []
        self._stderr_task: Optional[asyncio.Task] = None

    async def start(self) -> None:
        kwargs = {}
        if IS_WINDOWS:
            kwargs["creationflags"] = subprocess.CREATE_NEW_PROCESS_GROUP
        else:
            kwargs["start_new_session"] = True
        try:
            self.proc = await asyncio.create_subprocess_exec(
                *self.argv,
                cwd=str(self.cwd),
                env=self.env,
                stdin=asyncio.subprocess.PIPE if self.stdin_text is not None else asyncio.subprocess.DEVNULL,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                limit=16 * 1024 * 1024,  # one JSON event can carry a large tool output
                **kwargs,
            )
        except FileNotFoundError as exc:
            raise ProcessError(f"Command not found: {self.argv[0]}") from exc

        self._stderr_task = asyncio.create_task(self._drain_stderr())
        if self.stdin_text is not None and self.proc.stdin:
            try:
                self.proc.stdin.write(self.stdin_text.encode("utf-8"))
                await self.proc.stdin.drain()
            except (BrokenPipeError, ConnectionResetError):
                pass
            finally:
                self.proc.stdin.close()

    async def _drain_stderr(self) -> None:
        # Read continuously, or a chatty CLI blocks once the stderr pipe buffer fills.
        assert self.proc and self.proc.stderr
        while True:
            chunk = await self.proc.stderr.read(65536)
            if not chunk:
                break
            self._stderr_chunks.append(chunk.decode("utf-8", errors="replace"))

    async def lines(self) -> AsyncIterator[str]:
        """Yield stdout lines (without the newline) as the CLI prints them."""
        if self.proc is None:
            await self.start()
        assert self.proc and self.proc.stdout
        while True:
            raw = await self.proc.stdout.readline()
            if not raw:
                break
            line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
            if line:
                yield line

    async def wait(self) -> int:
        assert self.proc
        code = await self.proc.wait()
        if self._stderr_task:
            await self._stderr_task
        return code

    @property
    def stderr(self) -> str:
        return "".join(self._stderr_chunks)

    async def kill(self) -> None:
        """Kill the CLI and everything it started."""
        if not self.proc or self.proc.returncode is not None:
            return
        pid = self.proc.pid
        try:
            if IS_WINDOWS:
                killer = await asyncio.create_subprocess_exec(
                    "taskkill", "/PID", str(pid), "/T", "/F",
                    stdout=asyncio.subprocess.DEVNULL,
                    stderr=asyncio.subprocess.DEVNULL,
                )
                await killer.wait()
            else:
                os.killpg(os.getpgid(pid), signal.SIGKILL)
        except (ProcessLookupError, OSError):
            pass
        try:
            await asyncio.wait_for(self.proc.wait(), timeout=10)
        except asyncio.TimeoutError:
            self.proc.kill()


if IS_WINDOWS and sys.version_info < (3, 8):  # pragma: no cover
    raise ImportError("asyncio subprocesses need Python 3.8+ on Windows")

"""Run an approved shell command with a deadline and bounded retained output.

cwd is the project. A shell is not confined to cwd: approval grants normal OS access.
"""
import asyncio
import os
import signal
import subprocess
import json
import sys
import codecs
from pathlib import Path

from . import ToolResult


class ShellTool:
    def __init__(self, root: Path, timeout: float = 30, output_limit: int = 16000):
        self.root = Path(root).resolve()
        self.timeout = timeout
        self.output_limit = output_limit
        self.proc = None
        self._job = None

    async def cancel(self):
        if self._job is not None:
            self._job.close()
            self._job = None
        if self.proc is None:
            return
        try:
            if os.name == 'nt':
                if self.proc.returncode is None:
                    killer = await asyncio.create_subprocess_exec(
                        'taskkill', '/PID', str(self.proc.pid), '/T', '/F',
                        stdout=asyncio.subprocess.DEVNULL, stderr=asyncio.subprocess.DEVNULL)
                    await killer.wait()
            else:
                os.killpg(self.proc.pid, signal.SIGKILL)
        except (ProcessLookupError, OSError):
            pass
        await self.proc.wait()

    async def stream(self, command: str, timeout=None):
        """Yield bounded live text chunks, then one ToolResult for model history."""
        items = asyncio.Queue(maxsize=16)

        async def execute():
            try:
                result = await self.run(command, on_output=items.put, timeout=timeout)
            except Exception as exc:
                await items.put(exc)
            else:
                await items.put(result)

        task = asyncio.create_task(execute())
        try:
            while True:
                item = await items.get()
                if isinstance(item, BaseException):
                    raise item
                yield item
                if isinstance(item, ToolResult):
                    break
        finally:
            if not task.done():
                task.cancel()
            await asyncio.gather(task, return_exceptions=True)

    async def run(self, command: str, on_output=None, timeout=None) -> ToolResult:
        if not isinstance(command, str) or not command.strip():
            raise ValueError('command must be a non-empty string')
        if len(command.encode('utf-8')) > 128 * 1024:
            raise ValueError('Command exceeds 128 KiB limit')
        deadline = self.timeout if timeout is None else timeout
        if isinstance(deadline, bool) or not isinstance(deadline, (int, float)) or not 0 < deadline <= 600:
            raise ValueError('timeout must be greater than zero and at most 600 seconds')
        env = {k: v for k, v in os.environ.items() if not k.upper().endswith(('_KEY', '_TOKEN', '_SECRET'))}
        env['PYTHONIOENCODING'] = 'utf-8'
        # Windows venv python.exe is a redirector which may spawn the interpreter
        # before job assignment. The stdlib-only bootstrap must use the real base
        # interpreter so the process blocked on stdin is the process we own.
        bootstrap = getattr(sys, '_base_executable', None) or sys.executable
        argv = [bootstrap, '-u', str(Path(__file__).with_name('_shell_worker.py'))] if os.name == 'nt' else ['/bin/sh', '-c', command]
        kwargs = {'creationflags': subprocess.CREATE_NEW_PROCESS_GROUP} if os.name == 'nt' else {'start_new_session': True}
        output = bytearray()
        truncated = False
        decoder = codecs.getincrementaldecoder('utf-8')(errors='replace')

        async def consume():
            nonlocal truncated
            while True:
                chunk = await self.proc.stdout.read(4096)
                if not chunk:
                    break
                remaining = self.output_limit - len(output)
                retained = chunk[:max(0, remaining)]
                output.extend(retained)
                if on_output and retained:
                    text = decoder.decode(retained)
                    if text:
                        await on_output(text)
                truncated |= len(chunk) > remaining
            await self.proc.wait()

        timed_out = False
        try:
            if os.name == 'nt':
                from .windows_job import WindowsJob
                self._job = WindowsJob()
            self.proc = await asyncio.create_subprocess_exec(
                *argv, cwd=self.root, stdin=asyncio.subprocess.PIPE if os.name == 'nt' else asyncio.subprocess.DEVNULL,
                stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.STDOUT, env=env, **kwargs)
            if self._job is not None:
                self._job.assign(self.proc.pid)
                self.proc.stdin.write((json.dumps(command) + '\n').encode('utf-8'))
                await self.proc.stdin.drain()
                self.proc.stdin.close()
            try:
                await asyncio.wait_for(consume(), timeout=deadline)
            except asyncio.TimeoutError:
                timed_out = True
            text = output.decode('utf-8', errors='replace')
            if on_output:
                tail = decoder.decode(b'', final=True)
                if tail:
                    await on_output(tail)
            if truncated:
                text += '\n[output truncated]'
                if on_output:
                    await on_output('\n[output truncated]')
            if timed_out:
                text += f'\n[command timed out after {deadline}s]'
                if on_output:
                    await on_output(f'\n[command timed out after {deadline}s]')
            return ToolResult(text, ok=not timed_out and self.proc.returncode == 0)
        finally:
            await self.cancel()
            self.proc = None

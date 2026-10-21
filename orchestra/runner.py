"""
Script and Pipeline Execution Engine.
Executes Python scripts, shell commands, test suites, and automations with
cross-platform support (Windows/Linux/macOS), timeout enforcement, and structured output capture.
"""

from __future__ import annotations
import json
import os
import subprocess
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Dict, List, Optional


@dataclass
class ExecutionResult:
    command: str
    exit_code: int
    stdout: str
    stderr: str
    duration_seconds: float
    success: bool
    parsed_json: Optional[Dict[str, Any]] = None

    def to_dict(self) -> Dict[str, Any]:
        return {
            "command": self.command,
            "exit_code": self.exit_code,
            "stdout": self.stdout,
            "stderr": self.stderr,
            "duration_seconds": self.duration_seconds,
            "success": self.success,
            "parsed_json": self.parsed_json,
        }


class ScriptRunner:
    """Executes arbitrary commands and Python scripts cleanly."""

    def __init__(self, default_cwd: Optional[Path | str] = None) -> None:
        self.default_cwd = Path(default_cwd or os.getcwd()).resolve()

    def run_command(
        self,
        command: str | List[str],
        cwd: Optional[Path | str] = None,
        env: Optional[Dict[str, str]] = None,
        timeout_seconds: Optional[int] = 300,
        shell: Optional[bool] = None,
    ) -> ExecutionResult:
        """
        Execute a shell command or argument list.
        Auto-determines shell setting based on command type and OS.
        """
        working_dir = Path(cwd or self.default_cwd).resolve()
        run_env = os.environ.copy()
        if env:
            run_env.update(env)

        if shell is None:
            # If string on Windows, use shell=True to support builtins & npm
            shell = isinstance(command, str)

        cmd_display = command if isinstance(command, str) else " ".join(command)

        start_time = time.time()
        try:
            proc = subprocess.run(
                command,
                cwd=str(working_dir),
                env=run_env,
                shell=shell,
                capture_output=True,
                text=True,
                encoding="utf-8",
                errors="replace",
                timeout=timeout_seconds,
            )
            duration = round(time.time() - start_time, 3)
            exit_code = proc.returncode
            stdout = proc.stdout or ""
            stderr = proc.stderr or ""
            success = exit_code == 0
        except subprocess.TimeoutExpired as exc:
            duration = round(time.time() - start_time, 3)
            exit_code = -1
            stdout = exc.stdout or ""
            stderr = f"Command timed out after {timeout_seconds} seconds."
            success = False
        except Exception as exc:
            duration = round(time.time() - start_time, 3)
            exit_code = -1
            stdout = ""
            stderr = str(exc)
            success = False

        # Attempt to parse last valid JSON object from stdout
        parsed_json = self._extract_json(stdout)

        return ExecutionResult(
            command=cmd_display,
            exit_code=exit_code,
            stdout=stdout,
            stderr=stderr,
            duration_seconds=duration,
            success=success,
            parsed_json=parsed_json,
        )

    def run_python_script(
        self,
        script_path: Path | str,
        args: Optional[List[str]] = None,
        cwd: Optional[Path | str] = None,
        env: Optional[Dict[str, str]] = None,
        timeout_seconds: Optional[int] = 300,
    ) -> ExecutionResult:
        """Execute a Python script using the active python interpreter."""
        script_full_path = Path(script_path)
        if not script_full_path.is_absolute():
            script_full_path = (cwd or self.default_cwd) / script_full_path

        cmd = [sys.executable, str(script_full_path)]
        if args:
            cmd.extend(args)

        return self.run_command(
            command=cmd,
            cwd=cwd,
            env=env,
            timeout_seconds=timeout_seconds,
            shell=False,
        )

    @staticmethod
    def _extract_json(text: str) -> Optional[Dict[str, Any]]:
        """Try to parse the entire text as JSON, or find a JSON block in the output."""
        stripped = text.strip()
        if not stripped:
            return None

        # 1. Direct parse
        try:
            val = json.loads(stripped)
            if isinstance(val, dict):
                return val
        except Exception:
            pass

        # 2. Reverse line scanning for embedded JSON
        lines = stripped.splitlines()
        for i in range(len(lines) - 1, -1, -1):
            line = lines[i].strip()
            if line.startswith("{") and line.endswith("}"):
                try:
                    val = json.loads(line)
                    if isinstance(val, dict):
                        return val
                except Exception:
                    continue

        return None


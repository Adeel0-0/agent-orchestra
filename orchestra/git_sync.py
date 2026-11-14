"""
Git Automation & Branch Synchronization Engine.
Handles automatic staging, conventional commit formatting, remote rebase,
secondary branch fast-forwarding, and remote pushes.
"""

from __future__ import annotations
from dataclasses import dataclass
from pathlib import Path
from typing import List, Optional

from .runner import ScriptRunner


@dataclass
class GitStatus:
    branch: str
    head_sha: str
    is_clean: bool
    changed_files: List[str]


class GitEngine:
    """Encapsulates Git operations for multi-agent workflows."""

    def __init__(self, workspace_root: Path | str) -> None:
        self.workspace_root = Path(workspace_root).resolve()
        self.runner = ScriptRunner(default_cwd=self.workspace_root)
        self.last_error = ""  # stderr of the most recent failed git command

    def _git(self, *args: str) -> bool:
        res = self.runner.run_command(["git", *args], shell=False)
        if not res.success:
            self.last_error = (res.stderr or res.stdout).strip()[-2000:]
        return res.success

    def is_git_repo(self) -> bool:
        res = self.runner.run_command(["git", "rev-parse", "--is-inside-work-tree"], shell=False)
        return res.success and res.stdout.strip() == "true"

    def get_status(self) -> GitStatus:
        branch_res = self.runner.run_command(["git", "rev-parse", "--abbrev-ref", "HEAD"], shell=False)
        branch = branch_res.stdout.strip() if branch_res.success else "unknown"

        head_res = self.runner.run_command(["git", "rev-parse", "--short", "HEAD"], shell=False)
        head_sha = head_res.stdout.strip() if head_res.success else "unknown"

        status_res = self.runner.run_command(["git", "status", "--porcelain"], shell=False)
        lines = [l.strip() for l in status_res.stdout.splitlines() if l.strip()]

        changed_files = [line[3:].strip() for line in lines if len(line) > 3]
        is_clean = len(changed_files) == 0

        return GitStatus(
            branch=branch,
            head_sha=head_sha,
            is_clean=is_clean,
            changed_files=changed_files,
        )

    def stage_all(self) -> bool:
        return self._git("add", ".")

    def commit(self, message: str) -> bool:
        return self._git("commit", "-m", message)

    def fetch(self, remote: str = "origin") -> bool:
        return self._git("fetch", remote, "--prune")

    def rebase(self, upstream: str) -> bool:
        """Rebase onto upstream. On a conflict, abort so the tree is left as it was before the rebase."""
        if self._git("rebase", upstream):
            return True
        error = self.last_error
        self._git("rebase", "--abort")
        self.last_error = error
        return False

    def push(self, remote: str = "origin", branch: str = "main") -> bool:
        return self._git("push", remote, branch)

    def fast_forward_branch(self, target_branch: str, source_branch: str = "main", remote: str = "origin") -> bool:
        """
        Move remote target_branch up to local source_branch.
        The remote rejects anything but a fast-forward, and nothing is checked out,
        so the working tree agents are using never changes.
        """
        return self._git("push", remote, f"{source_branch}:refs/heads/{target_branch}")


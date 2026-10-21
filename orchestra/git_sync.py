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
        res = self.runner.run_command(["git", "add", "."], shell=False)
        return res.success

    def commit(self, message: str) -> bool:
        res = self.runner.run_command(["git", "commit", "-m", message], shell=False)
        return res.success

    def fetch(self, remote: str = "origin") -> bool:
        res = self.runner.run_command(["git", "fetch", remote, "--prune"], shell=False)
        return res.success

    def rebase(self, upstream: str) -> bool:
        res = self.runner.run_command(["git", "rebase", upstream], shell=False)
        return res.success

    def push(self, remote: str = "origin", branch: str = "main") -> bool:
        res = self.runner.run_command(["git", "push", remote, branch], shell=False)
        return res.success

    def fast_forward_branch(self, target_branch: str, source_branch: str = "main", remote: str = "origin") -> bool:
        """
        Fast-forward target_branch to source_branch and push to remote.
        Restores the original branch when complete.
        """
        status = self.get_status()
        orig_branch = status.branch

        # Switch to target
        res1 = self.runner.run_command(["git", "checkout", target_branch], shell=False)
        if not res1.success:
            return False

        # Fast forward merge
        res2 = self.runner.run_command(["git", "merge", source_branch, "--ff-only"], shell=False)

        # Push to remote if ff succeeded
        if res2.success:
            self.runner.run_command(["git", "push", remote, target_branch], shell=False)

        # Switch back to original branch
        self.runner.run_command(["git", "checkout", orig_branch], shell=False)
        return res2.success


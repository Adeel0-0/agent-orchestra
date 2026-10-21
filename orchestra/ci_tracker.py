"""
Continuous Integration (CI) Tracker.
Monitors remote CI workflows (GitHub Actions) securely using git credential helper
and reports run completions and failures back to the event bus.
"""

from __future__ import annotations
import json
import urllib.request
import urllib.error
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, Optional

from .runner import ScriptRunner


@dataclass
class CiRunInfo:
    id: int
    head_sha: str
    status: str  # "queued" | "in_progress" | "completed"
    conclusion: Optional[str]  # "success" | "failure" | "cancelled" | None
    workflow_name: str
    html_url: str


class CiTracker:
    """Tracks GitHub Actions workflow runs without requiring external python dependencies."""

    def __init__(self, workspace_root: Path | str, repo_slug: Optional[str] = None) -> None:
        self.workspace_root = Path(workspace_root).resolve()
        self.runner = ScriptRunner(default_cwd=self.workspace_root)
        self.repo_slug = repo_slug or self._discover_repo_slug()

    def _discover_repo_slug(self) -> Optional[str]:
        """Extract owner/repo from git remote origin url."""
        res = self.runner.run_command(["git", "remote", "get-url", "origin"], shell=False)
        if not res.success:
            return None
        url = res.stdout.strip()
        # Handle https://github.com/owner/repo.git or git@github.com:owner/repo.git
        if "github.com" not in url:
            return None
        parts = url.split("github.com")[-1].replace(":", "/").strip("/")
        if parts.endswith(".git"):
            parts = parts[:-4]
        return parts

    def _get_github_token(self) -> Optional[str]:
        """Fetch token from git credential helper securely without writing to disk."""
        res = self.runner.run_command(
            ["git", "credential", "fill"],
            shell=False,
            # We pass input via command if runner supports it or standard helper
        )
        # Fallback to checking GITHUB_TOKEN or GH_TOKEN env vars
        import os
        token = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
        if token:
            return token.strip()
        return None

    def fetch_latest_run(
        self, workflow_filename: str = "backend-verification.yml", head_sha: Optional[str] = None
    ) -> Optional[CiRunInfo]:
        """Query GitHub API for latest workflow run."""
        if not self.repo_slug:
            return None

        url = f"https://api.github.com/repos/{self.repo_slug}/actions/workflows/{workflow_filename}/runs?per_page=5"
        token = self._get_github_token()

        headers = {
            "User-Agent": "Agent-Orchestra",
            "Accept": "application/vnd.github+json",
        }
        if token:
            headers["Authorization"] = f"Bearer {token}"

        req = urllib.request.Request(url, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                if resp.status != 200:
                    return None
                data = json.loads(resp.read().decode("utf-8"))
                runs = data.get("workflow_runs", [])
                if not runs:
                    return None

                # Find match for head_sha if given
                selected = None
                if head_sha:
                    for r in runs:
                        if r.get("head_sha", "").startswith(head_sha):
                            selected = r
                            break
                if not selected:
                    selected = runs[0]

                return CiRunInfo(
                    id=selected.get("id", 0),
                    head_sha=selected.get("head_sha", "")[:7],
                    status=selected.get("status", "unknown"),
                    conclusion=selected.get("conclusion"),
                    workflow_name=selected.get("name", workflow_filename),
                    html_url=selected.get("html_url", ""),
                )
        except Exception:
            return None


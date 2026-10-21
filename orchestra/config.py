"""
Configuration management for Agent Orchestra.
Supports loading from JSON and YAML with automatic discovery and sensible defaults.
"""

from __future__ import annotations
import json
import os
from dataclasses import dataclass, field, asdict
from pathlib import Path
from typing import Any, Dict, List, Optional


@dataclass
class GitSyncConfig:
    enabled: bool = True
    main_branch: str = "main"
    sync_branches: List[str] = field(default_factory=list)
    remote: str = "origin"
    auto_commit: bool = True
    auto_push: bool = True
    commit_prefix: str = "chore(orchestra):"


@dataclass
class CiConfig:
    enabled: bool = False
    provider: str = "github_actions"  # "github_actions" | "none"
    workflow_name: str = "backend-verification.yml"
    poll_interval_seconds: int = 15
    timeout_seconds: int = 600


@dataclass
class PipelineConfig:
    verify_command: Optional[str] = None
    pre_claim_command: Optional[str] = None
    post_sync_command: Optional[str] = None
    on_error_command: Optional[str] = None


@dataclass
class OrchestraConfig:
    workspace_root: str = "."
    orchestra_dir: str = ".orchestra"
    coordination_file: Optional[str] = None  # e.g. "docs/coordination.md"
    watch_paths: List[str] = field(default_factory=lambda: ["."])
    ignore_patterns: List[str] = field(
        default_factory=lambda: [
            ".git/*",
            ".orchestra/*",
            "node_modules/*",
            "__pycache__/*",
            "*.pyc",
            "*.log",
            ".next/*",
            "dist/*",
            "build/*",
        ]
    )
    debounce_ms: int = 400
    lock_lease_seconds: int = 1800  # 30 minutes default lock lease
    git: GitSyncConfig = field(default_factory=GitSyncConfig)
    ci: CiConfig = field(default_factory=CiConfig)
    pipeline: PipelineConfig = field(default_factory=PipelineConfig)

    def to_dict(self) -> Dict[str, Any]:
        return asdict(self)

    def save(self, filepath: Path) -> None:
        filepath.parent.mkdir(parents=True, exist_ok=True)
        with open(filepath, "w", encoding="utf-8") as f:
            json.dump(self.to_dict(), f, indent=2)

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> OrchestraConfig:
        git_data = data.get("git", {})
        ci_data = data.get("ci", {})
        pipeline_data = data.get("pipeline", {})

        return cls(
            workspace_root=str(data.get("workspace_root", ".")),
            orchestra_dir=str(data.get("orchestra_dir", ".orchestra")),
            coordination_file=data.get("coordination_file"),
            watch_paths=list(data.get("watch_paths", ["."])),
            ignore_patterns=list(data.get("ignore_patterns", [])),
            debounce_ms=int(data.get("debounce_ms", 400)),
            lock_lease_seconds=int(data.get("lock_lease_seconds", 1800)),
            git=GitSyncConfig(**git_data) if isinstance(git_data, dict) else GitSyncConfig(),
            ci=CiConfig(**ci_data) if isinstance(ci_data, dict) else CiConfig(),
            pipeline=PipelineConfig(**pipeline_data) if isinstance(pipeline_data, dict) else PipelineConfig(),
        )


def load_config(root_dir: Optional[str | Path] = None) -> OrchestraConfig:
    """
    Search for configuration file in root_dir:
    1. orchestra.json
    2. orchestra.yaml / orchestra.yml
    3. .orchestra/config.json
    Falls back to default config if none found.
    """
    root = Path(root_dir or os.getcwd()).resolve()
    candidates = [
        root / "orchestra.json",
        root / "orchestra.yaml",
        root / "orchestra.yml",
        root / ".orchestra" / "config.json",
    ]

    for candidate in candidates:
        if candidate.is_file():
            try:
                if candidate.suffix in (".yaml", ".yml"):
                    try:
                        import yaml  # type: ignore
                        with open(candidate, "r", encoding="utf-8") as f:
                            raw = yaml.safe_load(f)
                            if isinstance(raw, dict):
                                cfg = OrchestraConfig.from_dict(raw)
                                cfg.workspace_root = str(root)
                                return cfg
                    except ImportError:
                        pass  # fallback to next candidate
                else:
                    with open(candidate, "r", encoding="utf-8") as f:
                        raw = json.load(f)
                        if isinstance(raw, dict):
                            cfg = OrchestraConfig.from_dict(raw)
                            cfg.workspace_root = str(root)
                            return cfg
            except Exception:
                continue

    # Default fallback
    cfg = OrchestraConfig()
    cfg.workspace_root = str(root)
    return cfg


"""
Configuration management for Agent Orchestra.
Supports loading from JSON and YAML with automatic discovery and sensible defaults.
"""

from __future__ import annotations
import json
import os
from dataclasses import dataclass, field, fields, asdict
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
        """
        Build a config from parsed JSON/YAML. Unknown keys and wrong section types raise
        ConfigError: a typo like "auto_psuh" must stop the daemon, not silently mean the default.
        Keys that are left out keep their defaults.
        """
        sections = {"git": GitSyncConfig, "ci": CiConfig, "pipeline": PipelineConfig}
        _reject_unknown_keys(data, cls, "")

        kwargs: Dict[str, Any] = {}
        for key, value in data.items():
            if key in sections:
                if not isinstance(value, dict):
                    raise ConfigError(f"'{key}' must be an object, got {type(value).__name__}")
                _reject_unknown_keys(value, sections[key], f"{key}.")
                kwargs[key] = sections[key](**value)
            else:
                kwargs[key] = value
        return cls(**kwargs)


class ConfigError(ValueError):
    """The configuration file exists but cannot be used."""


def _reject_unknown_keys(data: Dict[str, Any], schema: type, prefix: str) -> None:
    unknown = sorted(set(data) - {f.name for f in fields(schema)})
    if unknown:
        allowed = ", ".join(f.name for f in fields(schema))
        raise ConfigError(f"Unknown config key(s): {', '.join(prefix + k for k in unknown)}. Allowed: {allowed}")


def load_config(root_dir: Optional[str | Path] = None) -> OrchestraConfig:
    """
    Search for configuration file in root_dir:
    1. orchestra.json
    2. orchestra.yaml / orchestra.yml
    3. .orchestra/config.json
    The first file found is used; if it cannot be read or validated, ConfigError is raised.
    Falls back to the default config only when no file exists.
    workspace_root in the file is resolved relative to root_dir.
    """
    root = Path(root_dir or os.getcwd()).resolve()
    candidates = [
        root / "orchestra.json",
        root / "orchestra.yaml",
        root / "orchestra.yml",
        root / ".orchestra" / "config.json",
    ]

    for candidate in candidates:
        if not candidate.is_file():
            continue
        try:
            with open(candidate, "r", encoding="utf-8") as f:
                if candidate.suffix in (".yaml", ".yml"):
                    try:
                        import yaml  # type: ignore
                    except ImportError:
                        raise ConfigError(f"{candidate} needs PyYAML: pip install agent-orchestra[yaml]")
                    raw = yaml.safe_load(f)
                else:
                    raw = json.load(f)
            if not isinstance(raw, dict):
                raise ConfigError("top level must be an object")
            cfg = OrchestraConfig.from_dict(raw)
        except ConfigError as exc:
            raise ConfigError(f"{candidate}: {exc}") from None
        except Exception as exc:
            raise ConfigError(f"{candidate}: {exc}") from exc
        cfg.workspace_root = str((root / cfg.workspace_root).resolve())
        return cfg

    # Default fallback
    cfg = OrchestraConfig()
    cfg.workspace_root = str(root)
    return cfg


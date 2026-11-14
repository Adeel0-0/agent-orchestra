"""Small project-scoped tools for API players."""
from dataclasses import dataclass
from typing import Optional


@dataclass
class ToolResult:
    output: str
    ok: bool = True
    path: Optional[str] = None
    kind: Optional[str] = None

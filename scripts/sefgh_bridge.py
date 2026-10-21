#!/usr/bin/env python3
"""
SEFGH Bridge Script for Agent Orchestra.
Syncs SEFGH's existing `docs/coordination.md` and `scripts/agent-handoff.mjs`
with Agent Orchestra's IPC Event Bus and Lock Manager.
"""

import json
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent
COORD_FILE = ROOT / "docs" / "coordination.md"

def inspect_coordination():
    if not COORD_FILE.is_file():
        print(f"File not found: {COORD_FILE}", file=sys.stderr)
        return 1

    content = COORD_FILE.read_text(encoding="utf-8")
    m = re.search(r"^-\s+\*\*Active writer:\*\*\s*(.+)$", content, re.MULTILINE)
    active_writer = m.group(1).strip() if m else "unknown"

    print(json.dumps({
        "active_writer": active_writer,
        "is_released": active_writer.lower().startswith("none"),
        "coordination_file": str(COORD_FILE),
    }))
    return 0

if __name__ == "__main__":
    sys.exit(inspect_coordination())


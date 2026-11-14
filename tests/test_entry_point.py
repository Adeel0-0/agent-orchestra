"""`python -m orchestra` works, and importing the CLI module does not run it."""

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent


class TestEntryPoint(unittest.TestCase):
    def test_python_dash_m_runs(self):
        with tempfile.TemporaryDirectory() as tmp:
            res = subprocess.run(
                [sys.executable, "-m", "orchestra", "status", "--json"],
                cwd=tmp,
                env={**os.environ, "PYTHONPATH": str(REPO_ROOT)},
                capture_output=True,
                text=True,
                timeout=60,
            )
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertIn("state", json.loads(res.stdout))

    def test_importing_cli_does_not_run_it(self):
        res = subprocess.run(
            [sys.executable, "-c", "import orchestra.cli; print('imported')"],
            cwd=REPO_ROOT,
            capture_output=True,
            text=True,
            timeout=60,
        )
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(res.stdout.strip(), "imported")


if __name__ == "__main__":
    unittest.main()

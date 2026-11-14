import shutil
import sys
import tempfile
import time
import unittest
from pathlib import Path
from orchestra.runner import ScriptRunner

class TestRunner(unittest.TestCase):
    def setUp(self):
        self.runner = ScriptRunner()

    def test_run_command_echo(self):
        res = self.runner.run_command([sys.executable, "-c", "print('hello orchestra')"])
        self.assertTrue(res.success)
        self.assertIn("hello orchestra", res.stdout)
        self.assertEqual(res.exit_code, 0)

    def test_run_command_json_parse(self):
        res = self.runner.run_command([sys.executable, "-c", "import json; print(json.dumps({'status': 'ok', 'score': 100}))"])
        self.assertTrue(res.success)
        self.assertIsNotNone(res.parsed_json)
        self.assertEqual(res.parsed_json["status"], "ok")
        self.assertEqual(res.parsed_json["score"], 100)

    def test_run_command_timeout(self):
        res = self.runner.run_command([sys.executable, "-c", "import time; time.sleep(5)"], timeout_seconds=1)
        self.assertFalse(res.success)
        self.assertEqual(res.exit_code, -1)
        self.assertIn("timed out", res.stderr)

    def test_timeout_kills_grandchildren_of_a_shell_command(self):
        tmp = Path(tempfile.mkdtemp())
        marker = tmp / "grandchild_survived"
        # Grandchild: if it is still alive 3s later, it leaves a marker.
        (tmp / "grandchild.py").write_text(
            f"import time, pathlib; time.sleep(3); pathlib.Path({str(marker)!r}).write_text('alive')\n"
        )
        # Child: starts the grandchild (which inherits the output pipe), prints, then hangs.
        (tmp / "child.py").write_text(
            "import subprocess, sys, time\n"
            f"subprocess.Popen([sys.executable, {str(tmp / 'grandchild.py')!r}])\n"
            "print('partial output', flush=True)\n"
            "time.sleep(30)\n"
        )
        # A shell string, like pipeline commands ("npm run handoff"): shell -> python -> python.
        command = f'"{sys.executable}" "{tmp / "child.py"}"'

        started = time.time()
        res = self.runner.run_command(command, timeout_seconds=1)
        elapsed = time.time() - started

        self.assertFalse(res.success)
        self.assertLess(elapsed, 8, "timeout waited for a grandchild to finish")
        self.assertIsInstance(res.stdout, str)
        self.assertIn("partial output", res.stdout)
        time.sleep(4)
        self.assertFalse(marker.exists(), "grandchild kept running after the timeout")
        shutil.rmtree(tmp, ignore_errors=True)

if __name__ == "__main__":
    unittest.main()


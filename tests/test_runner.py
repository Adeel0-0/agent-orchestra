import sys
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

if __name__ == "__main__":
    unittest.main()


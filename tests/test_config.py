import json
import shutil
import tempfile
import unittest
from pathlib import Path
from orchestra.config import OrchestraConfig, load_config

class TestConfig(unittest.TestCase):
    def setUp(self):
        self.temp_dir = Path(tempfile.mkdtemp())

    def tearDown(self):
        shutil.rmtree(self.temp_dir, ignore_errors=True)

    def test_default_config(self):
        cfg = OrchestraConfig()
        self.assertEqual(cfg.workspace_root, ".")
        self.assertEqual(cfg.git.main_branch, "main")
        self.assertTrue(cfg.git.enabled)

    def test_save_and_load_config(self):
        cfg_file = self.temp_dir / "orchestra.json"
        cfg = OrchestraConfig(workspace_root=str(self.temp_dir))
        cfg.git.sync_branches = ["dev", "staging"]
        cfg.save(cfg_file)

        loaded = load_config(self.temp_dir)
        self.assertEqual(loaded.git.sync_branches, ["dev", "staging"])
        self.assertEqual(loaded.workspace_root, str(self.temp_dir.resolve()))

if __name__ == "__main__":
    unittest.main()


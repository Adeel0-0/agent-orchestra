import json
import shutil
import tempfile
import unittest
from pathlib import Path
from orchestra.config import ConfigError, OrchestraConfig, load_config

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

    def write_config(self, data):
        (self.temp_dir / "orchestra.json").write_text(json.dumps(data), encoding="utf-8")

    def test_typo_in_section_key_is_an_error_not_defaults(self):
        self.write_config({"git": {"enabled": False, "auto_psuh": False}})
        with self.assertRaises(ConfigError) as ctx:
            load_config(self.temp_dir)
        self.assertIn("git.auto_psuh", str(ctx.exception))

    def test_unknown_top_level_key_is_an_error(self):
        self.write_config({"verify_command": "pytest"})  # belongs under "pipeline"
        with self.assertRaisesRegex(ConfigError, "verify_command"):
            load_config(self.temp_dir)

    def test_malformed_json_is_an_error(self):
        (self.temp_dir / "orchestra.json").write_text('{"git": {', encoding="utf-8")
        with self.assertRaises(ConfigError):
            load_config(self.temp_dir)

    def test_section_must_be_an_object(self):
        self.write_config({"git": "yes"})
        with self.assertRaisesRegex(ConfigError, "'git' must be an object"):
            load_config(self.temp_dir)

    def test_omitted_keys_keep_defaults(self):
        self.write_config({"git": {"auto_push": False}})
        cfg = load_config(self.temp_dir)
        self.assertFalse(cfg.git.auto_push)
        self.assertEqual(cfg.git.main_branch, "main")
        self.assertIn("node_modules/*", cfg.ignore_patterns)

    def test_workspace_root_is_relative_to_config_dir(self):
        project = self.temp_dir / "repo"
        tool_dir = project / "agent-orchestra"
        tool_dir.mkdir(parents=True)
        (tool_dir / "orchestra.json").write_text(json.dumps({"workspace_root": ".."}), encoding="utf-8")
        self.assertEqual(load_config(tool_dir).workspace_root, str(project.resolve()))

    def test_no_config_file_uses_defaults(self):
        cfg = load_config(self.temp_dir)
        self.assertEqual(cfg.workspace_root, str(self.temp_dir.resolve()))
        self.assertIsNone(cfg.pipeline.verify_command)

if __name__ == "__main__":
    unittest.main()


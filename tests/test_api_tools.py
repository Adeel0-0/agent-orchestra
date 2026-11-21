"""API-player tools: real temporary files and subprocesses, no credentials."""
import asyncio
import json
import os
import sys
import tempfile
import subprocess
import unittest
from pathlib import Path


class FileToolsTests(unittest.TestCase):
    def setUp(self):
        from orchestra.tools.fs import FileTools
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.tools = FileTools(self.root)

    def test_write_read_edit_and_find(self):
        self.tools.execute('write_file', {'path': 'src/a.py', 'content': 'hello\nhello again\n'})
        self.tools.execute('edit_file', {'path': 'src/a.py', 'old': 'hello again', 'new': 'goodbye'})
        self.assertEqual(self.tools.execute('read_file', {'path': 'src/a.py'}).output, 'hello\ngoodbye\n')
        self.assertIn('src/a.py', self.tools.execute('glob', {'pattern': '**/*.py'}).output)
        self.assertIn('src/a.py:2:goodbye', self.tools.execute('grep', {'pattern': 'goodbye'}).output)
        self.assertIn('src/', self.tools.execute('list_dir', {}).output)

    def test_ambiguous_edit_changes_nothing(self):
        p = self.root / 'a.txt'
        p.write_text('same same', encoding='utf-8')
        with self.assertRaises(ValueError):
            self.tools.execute('edit_file', {'path': 'a.txt', 'old': 'same', 'new': 'x'})
        self.assertEqual(p.read_text(), 'same same')

    def test_glob_star_is_one_segment_and_double_star_includes_root(self):
        (self.root / 'main.py').write_text('root')
        (self.root / 'src').mkdir()
        (self.root / 'src' / 'a.py').write_text('nested')
        self.assertEqual(self.tools.execute('glob', {'pattern': '*.py'}).output, 'main.py')
        self.assertEqual(self.tools.execute('glob', {'pattern': '**/*.py'}).output.splitlines(), ['main.py', 'src/a.py'])

    def test_search_cap_is_visible_even_without_matches(self):
        from orchestra.tools.fs import FileTools
        for i in range(3):
            (self.root / f'{i}.txt').write_text('nothing')
        for tool, pattern in (('glob', '*.py'), ('grep', 'no match')):
            result = FileTools(self.root, scan_limit=2).execute(tool, {'pattern': pattern})
            self.assertIn('scan truncated after 2 files', result.output)

    def test_traversal_and_absolute_escape_are_rejected(self):
        for path in ('../outside.txt', str(self.root.parent / 'outside.txt')):
            with self.subTest(path=path), self.assertRaises(ValueError):
                self.tools.execute('write_file', {'path': path, 'content': 'bad'})

    def test_writes_cannot_modify_git_or_orchestra_metadata(self):
        for directory in ('.git', '.orchestra', 'nested/.git'):
            target = self.root / directory / 'config'
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text('original', encoding='utf-8')
            for tool, args in [('write_file', {'content': 'bad'}), ('edit_file', {'old': 'original', 'new': 'bad'})]:
                with self.subTest(directory=directory, tool=tool), self.assertRaises(ValueError):
                    self.tools.execute(tool, {'path': str(target), **args})
                self.assertEqual(target.read_text(), 'original')
        with self.assertRaises(ValueError):
            self.tools.execute('write_file', {'path': '.git/hooks/pre-commit', 'content': 'bad'})

    def test_hardlink_alias_cannot_modify_protected_metadata(self):
        protected = self.root / '.git' / 'config'
        protected.parent.mkdir()
        protected.write_text('original')
        alias = self.root / 'ordinary.txt'
        try:
            os.link(protected, alias)
        except OSError:
            self.skipTest('filesystem does not support hardlinks')
        for name, args in [('write_file', {'content': 'bad'}), ('edit_file', {'old': 'original', 'new': 'bad'})]:
            with self.subTest(name=name), self.assertRaises(ValueError):
                self.tools.execute(name, {'path': 'ordinary.txt', **args})
            self.assertEqual(protected.read_text(), 'original')

    def test_read_file_line_ranges_can_reach_late_content_and_report_remaining(self):
        (self.root / 'large.py').write_text(''.join(f'line-{i:05d}\n' for i in range(8000)), encoding='utf-8')
        result = self.tools.execute('read_file', {'path': 'large.py', 'offset': 7000, 'limit': 3}).output
        self.assertIn('line-07000', result)
        self.assertIn('line-07002', result)
        self.assertNotIn('line-07003', result)
        self.assertIn('997 lines remain', result)
        for args in ({'offset': -1}, {'limit': 0}, {'offset': True}, {'limit': 2.5}):
            with self.subTest(args=args), self.assertRaises(ValueError):
                self.tools.execute('read_file', {'path': 'large.py', **args})

    def test_symlink_escape_is_not_read_or_searched(self):
        with tempfile.TemporaryDirectory() as outside:
            (Path(outside) / 'secret.txt').write_text('secret')
            try:
                (self.root / 'link').symlink_to(outside, target_is_directory=True)
            except OSError:
                self.skipTest('creating symlinks requires permission on this machine')
            with self.assertRaises(ValueError):
                self.tools.execute('read_file', {'path': 'link/secret.txt'})
            self.assertNotIn('secret', self.tools.execute('grep', {'pattern': 'secret'}).output)

    def test_bad_arguments_and_large_reads_are_bounded(self):
        (self.root / 'large').write_text('x' * 20000)
        self.assertLess(len(self.tools.execute('read_file', {'path': 'large'}).output), 17000)
        for name, args in [('read_file', {'path': 4}), ('edit_file', {'path': 'large', 'old': '', 'new': 'x'}),
                           ('unknown', {})]:
            with self.subTest(name=name), self.assertRaises(ValueError):
                self.tools.execute(name, args)

    @unittest.skipUnless(os.name == 'nt', 'Windows device names and junctions')
    def test_windows_devices_and_stream_paths_are_refused(self):
        for path in ('NUL', 'NUL.txt', 'a.txt:secret'):
            with self.subTest(path=path), self.assertRaises(ValueError):
                self.tools.execute('write_file', {'path': path, 'content': 'bad'})

    @unittest.skipUnless(os.name == 'nt', 'Windows junction')
    def test_junction_escape_is_refused_without_symlink_privileges(self):
        with tempfile.TemporaryDirectory() as outside:
            (Path(outside) / 'secret.txt').write_text('secret')
            result = subprocess.run(['cmd.exe', '/c', 'mklink', '/J', str(self.root / 'junction'), outside],
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            with self.assertRaises(ValueError):
                self.tools.execute('read_file', {'path': 'junction/secret.txt'})
            self.assertNotIn('secret', self.tools.execute('grep', {'pattern': 'secret'}).output)


class PolicyTests(unittest.TestCase):
    def test_policy_modes_gate_writes_and_commands(self):
        from orchestra.tools.policy import needs_permission
        for mode, write, command in [('read-only', True, True), ('edit', False, True), ('full', False, False)]:
            self.assertFalse(needs_permission(mode, 'read_file'))
            self.assertEqual(needs_permission(mode, 'write_file'), write)
            self.assertEqual(needs_permission(mode, 'run_command'), command)
        with self.assertRaises(ValueError):
            needs_permission('invalid', 'write_file')


class ShellTests(unittest.IsolatedAsyncioTestCase):
    async def test_command_timeout_override_is_bounded(self):
        from orchestra.tools.shell import ShellTool
        with tempfile.TemporaryDirectory() as tmp:
            runner = ShellTool(Path(tmp), timeout=0.01)
            result = await runner.run('echo extended', timeout=10)
            self.assertTrue(result.ok, result.output)
            for invalid in (0, -1, 601, True, '60'):
                with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                    await runner.run('echo bad', timeout=invalid)

    @unittest.skipUnless(os.name == 'nt', 'Windows PowerShell output encoding')
    async def test_unicode_output_is_utf8(self):
        from orchestra.tools.shell import ShellTool
        with tempfile.TemporaryDirectory() as tmp:
            result = await ShellTool(Path(tmp)).run("Write-Output 'café ✓'")
            self.assertTrue(result.ok, result.output)
            self.assertIn('café ✓', result.output)

    async def test_shell_environment_excludes_secret_suffixes(self):
        from unittest.mock import patch
        from orchestra.tools.shell import ShellTool
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'REVIEW_TEST_TOKEN': 'do-not-inherit', 'REVIEW_TEST_KEY': 'do-not-inherit', 'REVIEW_PUBLIC': 'visible'}):
            script = Path(tmp) / 'envcheck.py'
            script.write_text("import os,json\nprint(json.dumps({k:os.environ.get(k) for k in ['REVIEW_TEST_TOKEN','REVIEW_TEST_KEY','REVIEW_PUBLIC']}))\n")
            command = ('& ' if os.name == 'nt' else '') + f'"{sys.executable}" "{script}"'
            result = await ShellTool(Path(tmp)).run(command)
            self.assertTrue(result.ok, result.output)
            self.assertEqual(json.loads(result.output), {'REVIEW_TEST_TOKEN': None, 'REVIEW_TEST_KEY': None, 'REVIEW_PUBLIC': 'visible'})
    async def test_close_does_not_deadlock_when_live_output_queue_is_full(self):
        from orchestra.tools.shell import ShellTool
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            script = root / 'chatty.py'
            script.write_text("import time\nfor i in range(1000):\n print('line',flush=True)\n time.sleep(.002)\n", encoding='utf-8')
            runner = ShellTool(root)
            command = ('& ' if os.name == 'nt' else '') + f'"{sys.executable}" "{script}"'
            stream = runner.stream(command)
            await asyncio.wait_for(anext(stream), 3)
            await asyncio.sleep(0.2)
            await asyncio.wait_for(stream.aclose(), 2)
            self.assertIsNone(runner.proc)

    async def test_output_streams_before_command_finishes_and_close_cancels(self):
        from orchestra.tools.shell import ShellTool
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            script = root / 'output.py'
            script.write_text("import time\nfrom pathlib import Path\nprint('started',flush=True)\ntime.sleep(5)\nPath('finished').write_text('done')\n", encoding='utf-8')
            runner = ShellTool(root)
            command = ('& ' if os.name == 'nt' else '') + f'"{sys.executable}" "{script}"'
            stream = runner.stream(command)
            try:
                first = await asyncio.wait_for(anext(stream), 3)
                self.assertIn('started', first)
                self.assertFalse((root / 'finished').exists())
            finally:
                await stream.aclose()
            self.assertIsNone(runner.proc)

    async def test_command_runs_in_project_and_caps_output(self):
        from orchestra.tools.shell import ShellTool
        with tempfile.TemporaryDirectory() as tmp:
            runner = ShellTool(Path(tmp), output_limit=200)
            # PowerShell and POSIX both support invoking an explicitly quoted executable.
            command = ('& ' if os.name == 'nt' else '') + f'"{sys.executable}" -c "print(\'x\'*10000)"'
            result = await runner.run(command)
            self.assertTrue(result.ok, result.output)
            self.assertLess(len(result.output), 300)
            self.assertIn('truncated', result.output)

    async def test_timeout_returns_failure(self):
        from orchestra.tools.shell import ShellTool
        with tempfile.TemporaryDirectory() as tmp:
            runner = ShellTool(Path(tmp), timeout=0.2)
            command = ('& ' if os.name == 'nt' else '') + f'"{sys.executable}" -c "import time; time.sleep(30)"'
            result = await runner.run(command)
            self.assertFalse(result.ok)
            self.assertIn('timed out', result.output)

    @unittest.skipUnless(os.name == 'nt', 'Windows process job ownership')
    async def test_timeout_kills_descendant_after_parent_has_exited(self):
        from orchestra.tools.shell import ShellTool
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            script = root / 'launch.py'
            pidfile = root / 'child.pid'
            script.write_text("import subprocess,sys\nfrom pathlib import Path\n"
                "p=subprocess.Popen([sys.executable,'-c','import time; time.sleep(30)'],stdout=sys.stdout,stderr=sys.stderr)\n"
                "Path('child.pid').write_text(str(p.pid))\n", encoding='utf-8')
            runner = ShellTool(root, timeout=2)
            result = await runner.run(f'& "{sys.executable}" "{script}"')
            self.assertFalse(result.ok)
            self.assertTrue(pidfile.exists(), result.output)
            pid = pidfile.read_text()
            state = subprocess.run(['tasklist', '/FI', f'PID eq {pid}', '/NH'], capture_output=True, text=True).stdout
            try:
                self.assertNotIn(pid, state, 'descendant survived command timeout')
            finally:
                # Exact test-owned PID cleanup on RED, never kill arbitrary processes.
                subprocess.run(['taskkill', '/PID', pid, '/T', '/F'], capture_output=True)


if __name__ == '__main__':
    unittest.main()

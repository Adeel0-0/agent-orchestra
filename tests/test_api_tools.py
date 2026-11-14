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

    def test_traversal_and_absolute_escape_are_rejected(self):
        for path in ('../outside.txt', str(self.root.parent / 'outside.txt')):
            with self.subTest(path=path), self.assertRaises(ValueError):
                self.tools.execute('write_file', {'path': path, 'content': 'bad'})

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

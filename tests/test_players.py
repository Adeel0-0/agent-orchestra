"""Phase 1: mappers against recorded CLI output, the process runner, and the player loop."""

import asyncio
import io
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

from orchestra.chat import Renderer
from orchestra.players import base
from orchestra.players.antigravity import AntigravityMapper, AntigravityPlayer
from orchestra.players.base import CliPlayer, Event
from orchestra.players.claude import ClaudeMapper, ClaudePlayer
from orchestra.players.codex import CodexMapper, CodexPlayer
from orchestra.players.process import ChildProcess, ProcessError

HERE = Path(__file__).resolve().parent
FIXTURES = HERE / "fixtures"
FAKE = str(HERE / "fake_cli.py")
PROJECT = Path("C:\\project") if os.name == "nt" else Path("/project")


def replay(mapper, name):
    events = []
    for line in (FIXTURES / name).read_text(encoding="utf-8").splitlines():
        if line.strip():
            events += mapper.feed(json.loads(line))
    return events


def types(events):
    return [e.type for e in events]


def text_of(events):
    return "".join(e.data["text"] for e in events if e.type == base.TEXT_DELTA)


def pid_alive(pid: int) -> bool:
    if os.name == "nt":
        out = subprocess.run(["tasklist", "/FI", f"PID eq {pid}", "/NH"], capture_output=True, text=True).stdout
        return str(pid) in out
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


class TestCodexMapper(unittest.TestCase):
    def test_edit_turn(self):
        m = CodexMapper(PROJECT)
        ev = replay(m, "codex_edit.jsonl")
        self.assertEqual(m.session_id, "01a1d9a8-e58b-7e22-8a89-cbe634014e35")
        self.assertEqual(ev[0].type, base.SESSION_STARTED)
        self.assertEqual(types(ev)[-2:], [base.USAGE, base.TURN_COMPLETED])
        self.assertIn("goodbye", text_of(ev))
        self.assertIn("\n\n", text_of(ev))  # two messages, kept apart

        shell = [e for e in ev if e.type == base.TOOL_STARTED and e.data["name"] == "shell"][0]
        self.assertEqual(shell.data["input"]["command"], "cat a.txt")
        self.assertIn("powershell", shell.data["input"]["raw_command"].lower())
        out = [e for e in ev if e.type == base.TOOL_OUTPUT][0]
        self.assertEqual(out.data["chunk"].strip(), "hello")

        changed = [e for e in ev if e.type == base.FILE_CHANGED]
        self.assertEqual([(e.data["path"], e.data["kind"]) for e in changed], [("a.txt", "update")])
        self.assertTrue(all(e.data["ok"] for e in ev if e.type == base.TOOL_FINISHED))
        self.assertEqual([e for e in ev if e.type == base.USAGE][0].data["output_tokens"], 167)

    def test_display_command_unwraps_shells(self):
        from orchestra.players.codex import display_command
        ps = '"C:\\\\WINDOWS\\\\System32\\\\WindowsPowerShell\\\\v1.0\\\\powershell.exe" -Command '
        self.assertEqual(display_command(ps + "'cat a.txt'"), "cat a.txt")
        self.assertEqual(display_command(ps + "'echo ''hi'''"), "echo 'hi'")
        self.assertEqual(display_command(ps + '"python -c \\"print(1)\\""'), 'python -c "print(1)"')
        self.assertEqual(display_command("/bin/bash -lc 'ls -la'"), "ls -la")
        self.assertEqual(display_command("git status"), "git status")

    def test_resume_turn(self):
        ev = replay(CodexMapper(PROJECT), "codex_resume.jsonl")
        self.assertEqual(text_of(ev), "goodbye")

    def test_error_turn_reports_once_and_keeps_warning_as_notice(self):
        m = CodexMapper(PROJECT)
        ev = replay(m, "codex_error.jsonl") + m.finish(1, "")
        self.assertEqual(types(ev).count(base.ERROR), 1)
        self.assertIn("not supported", [e for e in ev if e.type == base.ERROR][0].data["message"])
        self.assertEqual(types(ev).count(base.NOTICE), 1)
        self.assertEqual(ev[-1].data["stop_reason"], "error")
        self.assertEqual(types(ev).count(base.TURN_COMPLETED), 1)


class TestClaudeMapper(unittest.TestCase):
    def test_tool_turn(self):
        m = ClaudeMapper(PROJECT)
        ev = replay(m, "claude_tool.jsonl")
        self.assertEqual(m.session_id, "779a99ae-8bc5-428c-9bcb-6c0c54f66d21")
        self.assertEqual(ev[0].data["model"], "claude-opus-5-5")
        started = [e for e in ev if e.type == base.TOOL_STARTED][0]
        finished = [e for e in ev if e.type == base.TOOL_FINISHED][0]
        self.assertEqual(started.data["id"], finished.data["id"])
        self.assertTrue(finished.data["ok"])
        self.assertIn("goodbye", text_of(ev))
        self.assertEqual(ev[-1].type, base.TURN_COMPLETED)

    def test_edit_tool_reports_changed_file(self):
        m = ClaudeMapper(PROJECT)
        ev = m.feed({"type": "assistant", "message": {"content": [
            {"type": "tool_use", "id": "t1", "name": "Edit", "input": {"file_path": str(PROJECT / "src" / "x.py")}}]}})
        ev += m.feed({"type": "user", "message": {"content": [
            {"type": "tool_result", "tool_use_id": "t1", "content": "ok", "is_error": False}]}})
        changed = [e for e in ev if e.type == base.FILE_CHANGED]
        self.assertEqual(changed[0].data["path"], str(Path("src") / "x.py"))
        self.assertEqual(changed[0].data["kind"], "update")

    def test_write_that_creates_a_file_is_an_add(self):
        m = ClaudeMapper(PROJECT)
        m.feed({"type": "assistant", "message": {"content": [
            {"type": "tool_use", "id": "w1", "name": "Write", "input": {"file_path": str(PROJECT / "test_calc.py")}}]}})
        ev = m.feed({"type": "user", "message": {"content": [{
            "type": "tool_result", "tool_use_id": "w1", "is_error": False,
            "content": f"File created successfully at: {PROJECT / 'test_calc.py'}"}]}})
        changed = [e for e in ev if e.type == base.FILE_CHANGED]
        self.assertEqual((changed[0].data["path"], changed[0].data["kind"]), ("test_calc.py", "add"))

    def test_error_result(self):
        m = ClaudeMapper(PROJECT)
        ev = m.feed({"type": "result", "is_error": True, "result": "Invalid API key", "usage": {}})
        self.assertEqual(types(ev), [base.USAGE, base.ERROR, base.TURN_COMPLETED])


class TestAntigravityMapper(unittest.TestCase):
    """Recorded from agy 1.5.14 (tests/fixtures/agy_*.jsonl)."""

    def test_reply_keeps_the_done_tail(self):
        m = AntigravityMapper(PROJECT)
        ev = replay(m, "agy_pong.jsonl")
        self.assertEqual(m.session_id, "027c9fb3-dc8a-458e-b380-bc6e642e094d")
        self.assertEqual(ev[0].type, base.SESSION_STARTED)
        # "pong" arrives while ACTIVE and the trailing newline on DONE; both belong to the reply.
        self.assertEqual(text_of(ev), "pong\n")
        usage = [e for e in ev if e.type == base.USAGE][0].data
        self.assertEqual(usage["output_tokens"], 3134)  # already includes the 3133 thinking tokens
        self.assertEqual(ev[-1].data["stop_reason"], "end_turn")
        self.assertEqual(types(ev).count(base.TURN_COMPLETED), 1)

    def test_command_and_edit_turn(self):
        m = AntigravityMapper(PROJECT)
        ev = replay(m, "agy_edit.jsonl")
        started = [e for e in ev if e.type == base.TOOL_STARTED]
        self.assertEqual([e.data["name"] for e in started], ["run_command", "edit_file"])
        self.assertEqual(started[0].data["input"]["CommandLine"], "type a.txt")
        self.assertIn("hello", [e for e in ev if e.type == base.TOOL_OUTPUT][0].data["chunk"])
        self.assertTrue(all(e.data["ok"] for e in ev if e.type == base.TOOL_FINISHED))
        self.assertEqual([e.data["path"] for e in ev if e.type == base.FILE_CHANGED], ["a.txt"])
        # Streamed in 13 pieces; reassembled text equals the final response exactly.
        last = json.loads((FIXTURES / "agy_edit.jsonl").read_text(encoding="utf-8").splitlines()[-1])
        self.assertEqual(text_of(ev), last["result"]["response"])
        usage = [e for e in ev if e.type == base.USAGE][0].data
        self.assertEqual(usage["input_tokens"], 21827 + 29735)
        self.assertEqual(usage["cached_input_tokens"], 29735)

    def test_denied_command_is_explained_not_silent(self):
        m = AntigravityMapper(PROJECT)
        ev = replay(m, "agy_denied.jsonl")
        finished = [e for e in ev if e.type == base.TOOL_FINISHED]
        self.assertEqual(len(finished), 1)          # state "ERROR" still finishes the tool
        self.assertFalse(finished[0].data["ok"])
        self.assertIn("denied", [e for e in ev if e.type == base.TOOL_OUTPUT][0].data["chunk"])
        notices = [e.data["message"] for e in ev if e.type == base.NOTICE]
        self.assertIn("Permission denied: Bash", notices)
        self.assertTrue(any("--permission edit" in n for n in notices))
        self.assertEqual(text_of(ev), "")
        self.assertEqual(ev[-1].data["stop_reason"], "end_turn")

    def test_error_envelope(self):
        m = AntigravityMapper(PROJECT)
        ev = m.feed({"event": "result", "result": {"conversation_id": "", "status": "ERROR", "error": "invalid model selection"}})
        self.assertIsNone(m.session_id)
        self.assertEqual(types(ev), [base.USAGE, base.ERROR, base.TURN_COMPLETED])

    def test_cancelled_envelope(self):
        ev = AntigravityMapper(PROJECT).feed({"event": "result", "result": {"status": "CANCELED"}})
        self.assertEqual(ev[-1].data["stop_reason"], "cancelled")


class TestCommands(unittest.TestCase):
    def test_codex_new_and_resume(self):
        p = CodexPlayer(PROJECT, model="gpt-5", permission="read-only")
        argv, stdin = p.build_command("fix & test")
        self.assertEqual(stdin, "fix & test")
        self.assertNotIn("fix & test", argv)
        self.assertEqual(argv[argv.index("-s") + 1], "read-only")
        p.session_id = "abc"
        argv, _ = p.build_command("again")
        self.assertEqual(argv[:3], ["codex", "exec", "resume"])
        self.assertIn("sandbox_mode=read-only", argv)
        self.assertEqual(argv[-2:], ["abc", "-"])

    def test_claude_and_agy_permissions(self):
        claude_modes = {p: ClaudePlayer(PROJECT, permission=p).build_command("x")[0] for p in ("read-only", "edit", "full")}
        mode_of = {p: argv[argv.index("--permission-mode") + 1] for p, argv in claude_modes.items()}
        # edit = auto: acceptEdits refuses every shell command headlessly, so tests could not run.
        self.assertEqual(mode_of, {"read-only": "plan", "edit": "auto", "full": "bypassPermissions"})
        argv, stdin = AntigravityPlayer(PROJECT, permission="read-only", session_id="c1").build_command("-x & hi")
        self.assertNotIn("-x & hi", argv)                       # prompt never on the command line
        self.assertEqual(json.loads(stdin), {"event": "user", "message": {"content": "-x & hi"}})
        self.assertTrue(stdin.endswith("\n"))
        self.assertEqual(argv[argv.index("--input-format") + 1], "stream-json")
        self.assertEqual(argv[argv.index("--mode") + 1], "plan")
        self.assertEqual(argv[argv.index("--conversation") + 1], "c1")
        edit_argv = AntigravityPlayer(PROJECT).build_command("hi")[0]
        self.assertIn("--sandbox", edit_argv)                   # accept-edits denies every command headlessly
        self.assertNotIn("--mode", edit_argv)
        self.assertNotIn("--dangerously-skip-permissions", edit_argv)
        self.assertIn("--dangerously-skip-permissions",
                      AntigravityPlayer(PROJECT, permission="full").build_command("hi")[0])

    def test_bad_permission_rejected(self):
        with self.assertRaises(ValueError):
            CodexPlayer(PROJECT, permission="yolo")


class TestProcess(unittest.TestCase):
    def test_stdin_round_trip_with_shell_characters(self):
        async def go():
            child = ChildProcess([sys.executable, FAKE, "echo"], cwd=HERE, stdin_text="a & b | c %PATH% \"q\"")
            lines = [line async for line in child.lines()]
            return lines, await child.wait()

        lines, code = asyncio.run(go())
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(lines[0])["stdin"], "a & b | c %PATH% \"q\"")

    def test_unsafe_argument_to_cmd_shim_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            shim = Path(tmp) / "tool.cmd"
            shim.write_text("@echo off\n", encoding="utf-8")
            with self.assertRaises(ProcessError):
                ChildProcess([str(shim), "a&calc"], cwd=tmp)
            ChildProcess([str(shim), "--json", "sandbox_mode=read-only"], cwd=tmp)  # safe ones pass

    def test_missing_command(self):
        async def go():
            await ChildProcess(["definitely-not-a-real-cli-xyz"], cwd=HERE).start()

        with self.assertRaises(ProcessError):
            asyncio.run(go())


class FakeCodex(CliPlayer):
    """CodexPlayer's mapper, but the 'CLI' is tests/fake_cli.py."""

    id = "fake"
    executable = sys.executable
    args = []

    def build_command(self, prompt):
        return [sys.executable, FAKE, *self.args], None

    def new_mapper(self):
        return CodexMapper(self.cwd)


class TestPlayerLoop(unittest.TestCase):
    def run_player(self, args):
        player = FakeCodex(PROJECT if PROJECT.exists() else HERE)
        player.args = args

        async def go():
            return [e async for e in player.send("hi")]

        return player, asyncio.run(go())

    def test_full_turn_and_session_id(self):
        player, ev = self.run_player(["replay", str(FIXTURES / "codex_edit.jsonl")])
        self.assertEqual(player.session_id, "01a1d9a8-e58b-7e22-8a89-cbe634014e35")
        self.assertEqual(types(ev).count(base.TURN_COMPLETED), 1)

    def test_crash_without_turn_end_still_completes(self):
        with tempfile.TemporaryDirectory() as tmp:
            partial = Path(tmp) / "partial.jsonl"
            partial.write_text('{"type":"thread.started","thread_id":"t"}\n', encoding="utf-8")
            _, ev = self.run_player(["replay", str(partial), "3"])
        self.assertEqual(types(ev)[-2:], [base.ERROR, base.TURN_COMPLETED])
        self.assertIn("fake stderr line", ev[-2].data["message"])
        self.assertEqual(ev[-1].data["stop_reason"], "error")

    def test_cancel_kills_the_whole_tree(self):
        with tempfile.TemporaryDirectory() as tmp:
            pidfile = Path(tmp) / "grandchild.pid"
            player = FakeCodex(HERE)
            player.args = ["hang", str(pidfile)]

            async def go():
                async def consume():
                    async for evt in player.send("hi"):
                        if evt.type == base.SESSION_STARTED:
                            started.set()

                started = asyncio.Event()
                task = asyncio.create_task(consume())
                await asyncio.wait_for(started.wait(), timeout=30)
                task.cancel()  # what Ctrl+C does
                try:
                    await task
                except asyncio.CancelledError:
                    pass

            t0 = time.time()
            asyncio.run(go())
            self.assertLess(time.time() - t0, 30)
            grandchild = int(pidfile.read_text())
            for _ in range(50):
                if not pid_alive(grandchild):
                    break
                time.sleep(0.1)
            self.assertFalse(pid_alive(grandchild), "grandchild survived the cancel")


class TestRenderer(unittest.TestCase):
    def test_renders_a_turn_without_color(self):
        out = io.StringIO()
        r = Renderer(out=out, color=False)
        for e in replay(CodexMapper(PROJECT), "codex_edit.jsonl"):
            r.show(e)
        text = out.getvalue()
        self.assertIn("> shell", text)
        self.assertIn("~ a.txt (update)", text)
        self.assertIn("167 out tokens", text)
        self.assertNotIn("\033[", text)

    def _tool_line(self, tool_input, cwd=PROJECT):
        out = io.StringIO()
        Renderer(out=out, color=False, cwd=cwd).show(Event(base.TOOL_STARTED, {"id": "1", "name": "t", "input": tool_input}))
        return out.getvalue().rstrip("\n")

    def test_tool_paths_are_relative_to_the_project(self):
        # Antigravity's read/edit tools use "Path"; Claude's use "file_path".
        self.assertEqual(self._tool_line({"Path": str(PROJECT / "calc.py")}), "  > t calc.py")
        self.assertEqual(self._tool_line({"file_path": str(PROJECT / "src" / "x.py")}),
                         "  > t " + str(Path("src") / "x.py"))

    def test_long_outside_path_keeps_the_file_name(self):
        far = str(Path(PROJECT).parent / ("d" * 150) / "important.py")
        line = self._tool_line({"file_path": far})
        self.assertTrue(line.endswith("important.py"), line)
        self.assertIn("...", line)

    def test_multi_line_command_stays_on_one_line(self):
        line = self._tool_line({"command": "Get-Content calc.py\nGet-ChildItem -Name"})
        self.assertEqual(line, "  > t Get-Content calc.py ; Get-ChildItem -Name")

    def test_agy_write_file_reports_a_changed_file(self):
        m = AntigravityMapper(PROJECT)
        step = {"step_index": 3, "step_type": "tool", "tool_name": "write_file",
                "tool_info": {"name": "write_file", "parameters": {"Path": str(PROJECT / "test_calc.py")}}}
        ev = m.feed({"event": "step_update", "step_update": {**step, "state": "ACTIVE"}})
        ev += m.feed({"event": "step_update", "step_update": {**step, "state": "DONE"}})
        self.assertEqual([e.data["path"] for e in ev if e.type == base.FILE_CHANGED], ["test_calc.py"])

    def test_auth_error_shows_fix(self):
        out = io.StringIO()
        Renderer(out=out, color=False, login_hint="codex login").show(
            Event(base.ERROR, {"message": "not logged in", "kind": "auth"}))
        self.assertIn("fix: codex login", out.getvalue())


if __name__ == "__main__":
    unittest.main()

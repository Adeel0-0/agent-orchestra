import asyncio
import contextlib
import io
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from orchestra.chat import Renderer, run_turn
from orchestra.players.base import Event


class PermissionPlayer:
    def __init__(self):
        self.allowed = None

    async def send(self, prompt):
        yield Event('permission.requested', {'request_id': 'r', 'tool': 'run_command',
            'input': {'command': 'echo hello'}, 'reason': 'Approval required'})
        yield Event('text.delta', {'text': 'allowed' if self.allowed else 'denied'})
        yield Event('turn.completed', {'stop_reason': 'end_turn'})

    async def answer_permission(self, request_id, allow):
        self.allowed = allow


class ChatTests(unittest.TestCase):
    def test_json_denies_permission_without_polluting_json_output(self):
        player = PermissionPlayer()
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            ok = asyncio.run(run_turn(player, 'hi', None, True))
        self.assertTrue(ok)
        self.assertIs(player.allowed, False)
        import json
        events = [json.loads(line) for line in out.getvalue().splitlines()]
        self.assertEqual(events[1]['text'], 'denied')

    def test_terminal_permission_uses_explicit_yes(self):
        player = PermissionPlayer()
        out = io.StringIO()
        with patch('orchestra.chat.ask_permission', return_value=True):
            asyncio.run(run_turn(player, 'hi', Renderer(out=out, color=False), False))
        self.assertIs(player.allowed, True)
        self.assertIn('allowed', out.getvalue())

    def test_openrouter_discovery_does_not_require_cli_executable(self):
        from orchestra.players.registry import get_player
        self.assertEqual(get_player('openrouter').id, 'openrouter')

    def test_cli_missing_model_is_clean_error_not_traceback(self):
        from orchestra.cli import main
        out = io.StringIO()
        with tempfile.TemporaryDirectory() as tmp, contextlib.redirect_stderr(out):
            result = main(['chat', '-a', 'openrouter', '-C', tmp, 'hello'])
        self.assertEqual(result, 2)
        self.assertIn('--model', out.getvalue())

    def test_cost_is_visible_to_terminal_user(self):
        out = io.StringIO()
        renderer = Renderer(out=out, color=False)
        renderer.show(Event('usage', {'input_tokens': 5, 'output_tokens': 3, 'cost_usd': 0.0012}))
        renderer.show(Event('turn.completed', {'stop_reason': 'end_turn'}))
        self.assertIn('$0.0012', out.getvalue())


if __name__ == '__main__':
    unittest.main()

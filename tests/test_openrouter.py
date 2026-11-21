"""API loop against synthetic documented chunks, with real filesystem tools."""
import asyncio
import copy
import json
import tempfile
import unittest
from pathlib import Path

from orchestra.players import base


def text(value='done', usage=None):
    result = [{'choices': [{'delta': {'content': value}, 'finish_reason': None}]},
              {'choices': [{'delta': {}, 'finish_reason': 'stop'}]}]
    if usage:
        result.append({'choices': [], 'usage': usage})
    return result


def calls(*items):
    result = []
    for i, (name, args) in enumerate(items):
        encoded = json.dumps(args)
        result.extend([
            {'choices': [{'delta': {'tool_calls': [{'index': i, 'id': f'call{i}', 'type': 'function',
                'function': {'name': name, 'arguments': encoded[:5]}}]}, 'finish_reason': None}]},
            {'choices': [{'delta': {'tool_calls': [{'index': i, 'function': {'arguments': encoded[5:]}}]}, 'finish_reason': None}]}])
    result.append({'choices': [{'delta': {}, 'finish_reason': 'tool_calls'}]})
    return result


class ScriptedClient:
    def __init__(self, *responses, context=32000):
        self.responses = list(responses)
        self.requests = []
        self.context = context
        self.cancelled = False

    async def models(self):
        return [{'id': 'test/tools', 'context_length': self.context, 'supported_parameters': ['tools'],
                 'top_provider': {'max_completion_tokens': 8192}}]

    async def stream(self, body):
        self.requests.append(copy.deepcopy(body))
        if not self.responses:
            raise AssertionError('unexpected model request')
        for chunk in self.responses.pop(0):
            if isinstance(chunk, Exception):
                raise chunk
            yield chunk

    async def cancel(self):
        self.cancelled = True


class LoopTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def player(self, client, **kwargs):
        from orchestra.players.openrouter import OpenRouterPlayer
        return OpenRouterPlayer(self.root, model='test/tools', client=client, **kwargs)

    async def test_fragmented_calls_edit_then_followup_with_usage(self):
        client = ScriptedClient(calls(('write_file', {'path': 'a.txt', 'content': 'hello'})),
                                text('written', {'prompt_tokens': 10, 'completion_tokens': 4, 'cost': 0.002}),
                                text('hello', {'prompt_tokens': 12, 'completion_tokens': 2, 'cost': 0.001}))
        player = self.player(client)
        events = [e async for e in player.send('write hello')]
        self.assertEqual((self.root / 'a.txt').read_text(), 'hello')
        self.assertEqual(events[0].type, base.SESSION_STARTED)
        self.assertEqual(events[-1].data['stop_reason'], 'end_turn')
        self.assertEqual([e.data['path'] for e in events if e.type == base.FILE_CHANGED], ['a.txt'])
        result = client.requests[1]['messages'][-1]
        self.assertEqual(result['role'], 'tool')
        self.assertEqual(result['tool_call_id'], 'call0')
        events = [e async for e in player.send('what did you write?')]
        usage = next(e.data for e in events if e.type == base.USAGE)
        self.assertEqual(usage['session_input_tokens'], 22)
        self.assertAlmostEqual(usage['session_cost_usd'], 0.003)
        self.assertIn('write hello', json.dumps(client.requests[-1]['messages']))

    async def test_denied_command_returns_result_to_model_without_execution(self):
        client = ScriptedClient(calls(('run_command', {'command': 'echo unsafe > denied.txt'})), text())
        player = self.player(client)
        events = []
        async for e in player.send('run a command'):
            events.append(e)
            if e.type == 'permission.requested':
                self.assertEqual(e.data['tool'], 'run_command')
                await player.answer_permission(e.data['request_id'], False)
        self.assertFalse((self.root / 'denied.txt').exists())
        self.assertIn('denied', client.requests[1]['messages'][-1]['content'].lower())
        self.assertFalse(next(e.data['ok'] for e in events if e.type == base.TOOL_FINISHED))
        self.assertEqual(events[-1].data['stop_reason'], 'end_turn')

    async def test_ask_mode_write_pauses_until_approved(self):
        client = ScriptedClient(calls(('write_file', {'path': 'ok.txt', 'content': 'yes'})), text())
        player = self.player(client, permission='read-only')
        async for e in player.send('write'):
            if e.type == 'permission.requested':
                self.assertFalse((self.root / 'ok.txt').exists())
                await player.answer_permission(e.data['request_id'], True)
        self.assertEqual((self.root / 'ok.txt').read_text(), 'yes')

    async def test_bad_tool_and_escape_are_tool_failures_not_turn_crashes(self):
        client = ScriptedClient(calls(('write_file', {'path': '../bad', 'content': 'x'}),
                                     ('invented', {})), text())
        events = [e async for e in self.player(client).send('try')]
        self.assertEqual(len([e for e in events if e.type == base.TOOL_FINISHED and not e.data['ok']]), 2)
        self.assertEqual(events[-1].data['stop_reason'], 'end_turn')

    async def test_step_limit_finishes_with_error(self):
        client = ScriptedClient(calls(('list_dir', {})), calls(('list_dir', {})))
        events = [e async for e in self.player(client, max_steps=2).send('loop')]
        self.assertEqual(len(client.requests), 2)
        self.assertEqual(events[-1].data['stop_reason'], 'error')
        self.assertTrue(any(e.type == base.ERROR for e in events))

    async def test_stream_failure_does_not_execute_partial_tool_call(self):
        from orchestra.players.openrouter_client import OpenRouterError
        chunks = calls(('write_file', {'path': 'bad.txt', 'content': 'no'}))
        client = ScriptedClient(chunks[:-1] + [OpenRouterError('connection lost')])
        events = [e async for e in self.player(client).send('try')]
        self.assertFalse((self.root / 'bad.txt').exists())
        self.assertEqual(events[-1].data['stop_reason'], 'error')

    async def test_malformed_nested_stream_becomes_normalized_error(self):
        for chunk in ({'choices': ['invalid']}, {'choices': [{'delta': 'bad'}]},
                      {'choices': [{'delta': {'tool_calls': ['bad']}}]}):
            with self.subTest(chunk=chunk):
                events = [e async for e in self.player(ScriptedClient([chunk])).send('hi')]
                self.assertTrue(any(e.type == base.ERROR for e in events))
                self.assertEqual(events[-1].data['stop_reason'], 'error')

    async def test_context_includes_project_instructions(self):
        (self.root / 'AGENTS.md').write_text('Use unittest.', encoding='utf-8')
        client = ScriptedClient(text())
        _ = [e async for e in self.player(client).send('hi')]
        system = client.requests[0]['messages'][0]['content']
        self.assertIn('Use unittest.', system)
        self.assertIn(str(self.root), system)

    async def test_old_turns_summarized_without_breaking_tool_groups(self):
        client = ScriptedClient(text('a' * 36000), text('summary of older work'), text('next'), context=12000)
        player = self.player(client)
        _ = [e async for e in player.send('first')]
        events = [e async for e in player.send('second')]
        self.assertEqual(len(client.requests), 3)
        self.assertNotIn('tools', client.requests[1])
        self.assertIn('summary of older work', json.dumps(client.requests[2]['messages']))
        self.assertTrue(any(e.type == base.NOTICE for e in events))

    async def test_catalog_output_limit_is_used_instead_of_2048(self):
        client = ScriptedClient(text())
        _ = [e async for e in self.player(client).send('write a full module')]
        self.assertEqual(client.requests[0]['max_tokens'], 8192)

    async def test_summary_also_respects_small_model_output_ceiling(self):
        client = ScriptedClient(text('a' * 36000), text('summary'), text('continued'), context=12000)
        async def small_model():
            return [{'id': 'test/tools', 'context_length': 12000, 'top_provider': {'max_completion_tokens': 256}}]
        client.models = small_model
        player = self.player(client)
        _ = [e async for e in player.send('first')]
        _ = [e async for e in player.send('continue')]
        self.assertEqual(client.requests[1]['max_tokens'], 256)

    async def test_length_recovery_discards_partial_tools_then_accepts_smaller_write(self):
        truncated = calls(('write_file', {'path': 'partial.txt', 'content': 'never write'}))
        truncated[-1]['choices'][0]['finish_reason'] = 'length'
        client = ScriptedClient(truncated, calls(('write_file', {'path': 'safe.txt', 'content': 'small'})), text())
        events = [e async for e in self.player(client).send('write')]
        self.assertFalse((self.root / 'partial.txt').exists())
        self.assertEqual((self.root / 'safe.txt').read_text(), 'small')
        self.assertEqual(events[-1].data['stop_reason'], 'end_turn')
        self.assertIn('split', client.requests[1]['messages'][-1]['content'].lower())
        self.assertFalse(any(m.get('tool_calls') for m in client.requests[1]['messages']))

    async def test_large_instruction_files_fit_fresh_32k_context(self):
        for name in ('AGENTS.md', 'CLAUDE.md'):
            (self.root / name).write_text('Use ordinary project rules. ' * 600, encoding='utf-8')
        client = ScriptedClient(text(), context=32000)
        events = [e async for e in self.player(client).send('hello')]
        self.assertEqual(events[-1].data['stop_reason'], 'end_turn')
        self.assertEqual(len(client.requests), 1)

    async def test_context_estimate_uses_reported_prompt_tokens(self):
        client = ScriptedClient(text('ok', {'prompt_tokens': 20000}), text('summary'), text('continued'), context=24000)
        player = self.player(client)
        _ = [e async for e in player.send('start')]
        events = [e async for e in player.send('continue')]
        self.assertEqual(len(client.requests), 3)
        self.assertTrue(any(e.type == base.NOTICE for e in events))

    async def test_prompt_measurement_survives_player_eviction_and_restore(self):
        original = self.player(ScriptedClient(text('ok', {'prompt_tokens': 20000}), context=24000))
        _ = [e async for e in original.send('start')]
        resumed_client = ScriptedClient(text('summary'), text('continued'), context=24000)
        resumed = self.player(resumed_client)
        resumed.restore_state(original.snapshot_state())
        events = [e async for e in resumed.send('continue')]
        self.assertEqual(len(resumed_client.requests), 2)
        self.assertTrue(any(e.type == base.NOTICE for e in events))

    async def test_cancel_pending_permission_preserves_valid_followup_history(self):
        client = ScriptedClient(calls(('run_command', {'command': 'echo x'})), text())
        player = self.player(client)
        events = []
        async for e in player.send('do it'):
            events.append(e)
            if e.type == 'permission.requested':
                await player.cancel()
        self.assertEqual(events[-1].data['stop_reason'], 'cancelled')
        self.assertTrue(client.cancelled)
        _ = [e async for e in player.send('continue')]
        messages = client.requests[-1]['messages']
        results = [m for m in messages if m['role'] == 'tool']
        self.assertEqual(len(results), 1)
        self.assertIn('cancel', results[0]['content'].lower())

    async def test_resume_is_explicitly_unsupported_until_session_storage(self):
        with self.assertRaisesRegex(ValueError, 'resume'):
            self.player(ScriptedClient(), session_id='old')

    async def test_persisted_snapshot_restores_history_usage_without_credentials(self):
        client = ScriptedClient(text('remembered', {'prompt_tokens': 10, 'completion_tokens': 2, 'cost': 0.001}))
        original = self.player(client)
        _ = [e async for e in original.send('remember this')]
        snapshot = json.loads(json.dumps(original.snapshot_state()))
        self.assertEqual(set(snapshot), {'history', 'totals', 'context_length', 'prompt_measurement'})
        resumed_client = ScriptedClient(text('continued'))
        resumed = self.player(resumed_client)
        resumed.restore_state(snapshot)
        events = [e async for e in resumed.send('continue')]
        self.assertIn('remember this', json.dumps(resumed_client.requests[0]))
        usage = next(e.data for e in events if e.type == base.USAGE)
        self.assertEqual(usage['session_input_tokens'], 10)

    async def test_restore_repairs_unanswered_tools_without_reexecuting(self):
        player = self.player(ScriptedClient(text()))
        player.restore_state({'history': [{'role': 'user', 'content': 'do something'},
            {'role': 'assistant', 'content': None, 'tool_calls': [{'id': 'old', 'type': 'function',
                'function': {'name': 'write_file', 'arguments': '{"path":"danger","content":"no"}'}}]}],
            'totals': {'input_tokens': 0, 'output_tokens': 0, 'cost_usd': 0}, 'context_length': 32000})
        _ = [e async for e in player.send('continue')]
        results = [m for m in player.client.requests[0]['messages'] if m['role'] == 'tool']
        self.assertIn('interrupted', results[0]['content'].lower())
        self.assertFalse((self.root / 'danger').exists())


if __name__ == '__main__':
    unittest.main()

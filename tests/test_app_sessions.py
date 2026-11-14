import asyncio
import tempfile
import unittest
from pathlib import Path

from orchestra.players.base import Event
from orchestra.app.store import Store


class ControlledPlayer:
    """A real in-process producer whose lifetime is independent of any subscriber."""
    def __init__(self):
        self.started = asyncio.Event()
        self.proceed = asyncio.Event()
        self.session_id = 'backend-one'
        self.cancelled = False
        self.history = []
        self.permission = False

    async def send(self, prompt):
        self.history.append(prompt)
        yield Event('session.started', {'backend_session_id': self.session_id, 'model': None})
        self.started.set()
        yield Event('text.delta', {'text': 'first'})
        if self.permission:
            yield Event('permission.requested', {'request_id': 'approval', 'tool': 'run_command', 'input': {'command': 'echo test'}, 'reason': 'test'})
        await self.proceed.wait()
        yield Event('text.delta', {'text': 'last'})
        yield Event('turn.completed', {'stop_reason': 'end_turn'})

    async def answer_permission(self, rid, allow):
        self.history.append('allowed' if allow else 'denied')
        self.proceed.set()

    async def cancel(self):
        self.cancelled = True

    def snapshot_state(self):
        return {'history': self.history[:]}

    def restore_state(self, state):
        self.history = state['history'][:]


class SessionTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        from orchestra.app.sessions import SessionManager
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.project = self.root / 'project'
        self.project.mkdir()
        self.store = Store(self.root / 'data')
        self.player = ControlledPlayer()
        self.constructed = []

        def factory(session, cwd):
            self.constructed.append((session, cwd))
            return self.player
        self.factory = factory
        self.manager = SessionManager(self.store, factory=factory)
        p = self.store.add_project(str(self.project))
        self.sid = self.store.create_session(p['id'], 'codex', None, 'edit')['id']

    async def asyncTearDown(self):
        await self.manager.close()
        self.store.close()

    async def test_disconnect_reconnect_replays_and_turn_continues(self):
        self.manager.start(self.sid, 'hello')
        stream = self.manager.events(self.sid, after=0)
        first = await anext(stream)
        self.assertEqual(first['type'], 'message.user')
        await stream.aclose()  # UI disappears, agent must remain running.
        await self.player.started.wait()
        self.assertFalse(self.player.cancelled)
        self.player.proceed.set()
        await self.manager.wait(self.sid)
        replay = self.manager.events(self.sid, after=first['seq'])
        records = []
        async for event in replay:
            records.append(event)
            if event['type'] == 'turn.completed':
                break
        await replay.aclose()
        self.assertEqual([r['text'] for r in records if r['type'] == 'text.delta'], ['first', 'last'])
        self.assertEqual(self.store.session(self.sid)['backend_session_id'], 'backend-one')

    async def test_concurrent_turns_and_same_project_sessions_are_blocked(self):
        from orchestra.app.sessions import Conflict
        self.manager.start(self.sid, 'first')
        with self.assertRaises(Conflict):
            self.manager.start(self.sid, 'second')
        session = self.store.session(self.sid)
        other = self.store.create_session(session['project_id'], 'claude', None, 'edit')
        with self.assertRaises(Conflict):
            self.manager.start(other['id'], 'also writes')

    async def test_cancel_before_task_starts_releases_writer_and_can_restart(self):
        from orchestra.bus import LockManager
        self.manager.start(self.sid, 'cancel immediately')
        await self.manager.cancel(self.sid)
        self.assertEqual(self.store.session(self.sid)['status'], 'cancelled')
        self.assertFalse(LockManager(self.project / '.orchestra').is_locked())
        self.player.proceed.set()
        self.manager.start(self.sid, 'next')
        await self.manager.wait(self.sid)
        self.assertEqual(self.store.session(self.sid)['status'], 'idle')

    async def test_error_without_terminal_event_marks_session_error(self):
        async def incomplete(prompt):
            yield Event('error', {'message': 'backend failed', 'kind': 'other', 'retryable': False})
        self.player.send = incomplete
        self.manager.start(self.sid, 'fail')
        await self.manager.wait(self.sid)
        self.assertEqual(self.store.session(self.sid)['status'], 'error')
        self.assertEqual(self.store.events(self.sid)[-1]['stop_reason'], 'error')

    async def test_completed_player_is_evicted_and_reconstructed_for_next_turn(self):
        self.player.proceed.set()
        self.manager.start(self.sid, 'first')
        await self.manager.wait(self.sid)
        self.assertNotIn(self.sid, self.manager._players)
        self.manager.start(self.sid, 'second')
        await self.manager.wait(self.sid)
        self.assertEqual(len(self.constructed), 2)
        self.assertEqual(self.player.history, ['first', 'second'])

    async def test_unchanged_history_does_not_snapshot_each_text_delta(self):
        calls = []
        snapshot = self.player.snapshot_state
        def tracked_snapshot():
            calls.append(True)
            return snapshot()
        self.player.snapshot_state = tracked_snapshot
        self.player.snapshot_revision = lambda: tuple(self.player.history)
        self.manager.start(self.sid, 'stream')
        await self.player.started.wait()
        for _ in range(10):
            self.manager.publish(self.sid, {'type': 'text.delta', 'text': 'x'})
        self.assertLessEqual(len(calls), 2)

    async def test_cleanup_failure_blocks_project_until_explicit_confirmation(self):
        from orchestra.app.sessions import Conflict
        async def failed_cleanup():
            raise OSError('cleanup failed')
        self.player.cancel = failed_cleanup
        self.player.proceed.set()
        self.manager.start(self.sid, 'run')
        await self.manager.wait(self.sid)
        self.assertEqual(self.store.session(self.sid)['status'], 'cleanup_failed')
        with self.assertRaises(Conflict):
            self.manager.start(self.sid, 'unsafe retry')
        await self.manager.cancel(self.sid, confirm_stopped=True)
        self.assertEqual(self.store.session(self.sid)['status'], 'cancelled')

    async def test_immediate_cancel_cleanup_failure_is_durable_and_blocked(self):
        async def failed_cleanup():
            raise OSError('cleanup failed')
        self.player.cancel = failed_cleanup
        self.manager.start(self.sid, 'cancel before start')
        await self.manager.cancel(self.sid)
        self.assertEqual(self.store.session(self.sid)['status'], 'cleanup_failed')
        self.assertTrue(any(e['type'] == 'turn.completed' for e in self.store.events(self.sid)))
        await self.manager.cancel(self.sid, confirm_stopped=True)

    async def test_missing_project_is_validation_failure_before_player_starts(self):
        self.project.rmdir()
        with self.assertRaises(ValueError):
            self.manager.start(self.sid, 'missing project')
        self.assertEqual(self.constructed, [])

    async def test_heartbeat_exception_cancels_turn_even_if_reporting_fails(self):
        from unittest.mock import patch
        class FailedLock:
            def heartbeat(self, *args, **kwargs):
                raise OSError('disk unavailable')
        original_publish = self.manager.publish
        async def sleeper(_):
            return
        async def cancellable():
            await asyncio.Event().wait()
        task = asyncio.create_task(cancellable())
        self.manager._tasks[self.sid] = task
        try:
            with patch('orchestra.app.sessions.asyncio.sleep', sleeper), patch.object(self.manager, 'publish', side_effect=OSError('journal unavailable')):
                await self.manager._heartbeat(self.sid, FailedLock(), 'owner')
            await asyncio.sleep(0)
            self.assertTrue(task.cancelled())
        finally:
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)

    async def test_permission_and_cancel_are_session_scoped(self):
        self.player.permission = True
        self.manager.start(self.sid, 'ask')
        await self.player.started.wait()
        await asyncio.sleep(0)
        await self.manager.answer(self.sid, 'approval', False)
        await self.manager.wait(self.sid)
        self.assertIn('denied', self.player.history)
        self.player.proceed.clear()
        self.manager.start(self.sid, 'cancel')
        await asyncio.sleep(0)
        await self.manager.cancel(self.sid)
        self.assertTrue(self.player.cancelled)
        self.assertEqual(self.store.events(self.sid)[-1]['stop_reason'], 'cancelled')

    async def test_restart_restores_player_state_and_backend_id(self):
        from orchestra.app.sessions import SessionManager
        self.player.proceed.set()
        self.manager.start(self.sid, 'remember this')
        await self.manager.wait(self.sid)
        await self.manager.close()
        self.player = ControlledPlayer()
        self.player.proceed.set()
        self.manager = SessionManager(self.store, factory=self.factory)
        self.manager.start(self.sid, 'follow up')
        await self.manager.wait(self.sid)
        self.assertEqual(self.player.history, ['remember this', 'follow up'])
        self.assertEqual(self.constructed[-1][0]['backend_session_id'], 'backend-one')

    async def test_interrupted_restart_requires_stopped_process_confirmation(self):
        from orchestra.app.sessions import SessionManager, Conflict
        from orchestra.bus import LockManager
        self.store.update_session(self.sid, status='running')
        old_lock = LockManager(self.project / '.orchestra')
        old_lock.acquire(self.sid, 'session-' + self.sid + '-old-launch', lease_seconds=120)
        await self.manager.close()
        self.store.close()
        self.store = Store(self.root / 'data')
        self.manager = SessionManager(self.store, factory=self.factory)
        with self.assertRaises(Conflict):
            self.manager.start(self.sid, 'resume without inspection')
        await self.manager.cancel(self.sid, confirm_stopped=True)
        self.player.proceed.set()
        self.manager.start(self.sid, 'continue after inspection')
        await self.manager.wait(self.sid)
        self.assertEqual(self.store.session(self.sid)['status'], 'idle')

    async def test_slow_subscriber_cannot_stall_turn_and_can_replay(self):
        from orchestra.app.sessions import SlowSubscriber
        stream = self.manager.events(self.sid)
        pending = asyncio.create_task(anext(stream))
        await asyncio.sleep(0)
        self.manager.start(self.sid, 'start')
        await pending
        for _ in range(200):
            self.manager.publish(self.sid, {'type': 'text.delta', 'text': 'x'})
        with self.assertRaises(SlowSubscriber):
            _ = [e async for e in stream]


if __name__ == '__main__':
    unittest.main()

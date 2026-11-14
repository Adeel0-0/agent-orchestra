"""Real local HTTP/WebSocket acceptance with a fake backend, no model calls."""
import asyncio
import tempfile
import unittest
import importlib.util
from pathlib import Path

HAS_APP = all(importlib.util.find_spec(name) is not None for name in ('httpx', 'starlette', 'uvicorn', 'websockets'))
if HAS_APP:
    import httpx
    import uvicorn
    from websockets.asyncio.client import connect
    from orchestra.app.server import create_app
from orchestra.app.store import Store
from orchestra.players.base import Event

TOKEN = 'local-integration-test-token-not-a-credential'


@unittest.skipUnless(HAS_APP, 'Install .[test-app] for socket acceptance')
class LiveTests(unittest.IsolatedAsyncioTestCase):
    async def test_disconnect_replay_restart_and_followup_over_real_sockets(self):
        from orchestra.app.launcher import reserve_socket
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            project = root / 'project'
            project.mkdir()
            data = root / 'data'
            gate = asyncio.Event()
            received = []

            class FakePlayer:
                def __init__(self, session):
                    self.session_id = session['backend_session_id'] or 'real-socket-backend'
                async def send(self, prompt):
                    received.append((prompt, self.session_id))
                    yield Event('session.started', {'backend_session_id': self.session_id, 'model': None})
                    yield Event('text.delta', {'text': 'first'})
                    await gate.wait()
                    yield Event('text.delta', {'text': 'last'})
                    yield Event('turn.completed', {'stop_reason': 'end_turn'})
                async def cancel(self):
                    pass

            async def start():
                sock = reserve_socket()
                port = sock.getsockname()[1]
                app = create_app(store=Store(data), port=port, token=TOKEN, factory=lambda session, cwd: FakePlayer(session))
                server = uvicorn.Server(uvicorn.Config(app, log_level='critical', access_log=False, lifespan='on'))
                server.install_signal_handlers = lambda: None
                task = asyncio.create_task(server.serve(sockets=[sock]))
                try:
                    async def ready():
                        while not server.started:
                            if task.done():
                                await task
                                raise AssertionError('Server failed to start')
                            await asyncio.sleep(.01)
                    await asyncio.wait_for(ready(), 5)
                except BaseException:
                    server.should_exit = True
                    await task
                    sock.close()
                    raise
                return sock, port, server, task

            sock, port, server, task = await start()
            try:
                async with httpx.AsyncClient(base_url=f'http://127.0.0.1:{port}', headers={'Authorization': 'Bearer ' + TOKEN}) as client:
                    pid = (await client.post('/api/projects', json={'path': str(project)})).json()['id']
                    sid = (await client.post(f'/api/projects/{pid}/sessions', json={'player': 'codex'})).json()['id']
                    uri = f'ws://127.0.0.1:{port}/api/sessions/{sid}/events'
                    protocols = ['orchestra', 'orchestra.token.' + TOKEN]
                    async with connect(uri, subprotocols=protocols, origin=f'http://127.0.0.1:{port}') as ws:
                        result = await client.post(f'/api/sessions/{sid}/messages', json={'prompt': 'first turn'})
                        self.assertEqual(result.status_code, 202, result.text)
                        import json
                        first = json.loads(await asyncio.wait_for(ws.recv(), 3))
                    # The UI disconnects while the player is blocked, then the turn finishes.
                    gate.set()
                    await server.config.app.state.manager.wait(sid)
                    async with connect(uri + '?after=' + str(first['seq']), subprotocols=protocols,
                                       origin=f'http://127.0.0.1:{port}') as ws:
                        records = []
                        while not records or records[-1]['type'] != 'turn.completed':
                            records.append(json.loads(await asyncio.wait_for(ws.recv(), 3)))
                        self.assertEqual([e['text'] for e in records if e['type'] == 'text.delta'], ['first', 'last'])
            finally:
                server.should_exit = True
                await asyncio.wait_for(task, 5)
                sock.close()

            sock, port, server, task = await start()
            try:
                async with httpx.AsyncClient(base_url=f'http://127.0.0.1:{port}', headers={'Authorization': 'Bearer ' + TOKEN}) as client:
                    sessions = (await client.get(f'/api/projects/{pid}/sessions')).json()
                    self.assertEqual(sessions[0]['backend_session_id'], 'real-socket-backend')
                    result = await client.post(f'/api/sessions/{sid}/messages', json={'prompt': 'follow up'})
                    self.assertEqual(result.status_code, 202)
                    await server.config.app.state.manager.wait(sid)
                    self.assertEqual(received[-1], ('follow up', 'real-socket-backend'))
            finally:
                server.should_exit = True
                await asyncio.wait_for(task, 5)
                sock.close()


if __name__ == '__main__':
    unittest.main()

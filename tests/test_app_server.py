import asyncio
import tempfile
import unittest
import importlib.util
import subprocess
from pathlib import Path

HAS_APP = all(importlib.util.find_spec(name) is not None for name in ('httpx', 'starlette'))
if HAS_APP:
    import httpx

from orchestra.app.store import Store

TOKEN = 'test-launch-token-that-is-not-a-real-secret'
PORT = 8765


@unittest.skipUnless(HAS_APP, 'Install .[test-app] for API tests')
class ApiTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        from orchestra.app.server import create_app
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.project = self.root / 'project'
        self.project.mkdir()
        self.store = Store(self.root / 'data')
        self.app = create_app(store=self.store, port=PORT, token=TOKEN)
        self.lifespan = self.app.router.lifespan_context(self.app)
        await self.lifespan.__aenter__()
        self.client = httpx.AsyncClient(transport=httpx.ASGITransport(app=self.app),
            base_url=f'http://127.0.0.1:{PORT}', headers={'Authorization': 'Bearer ' + TOKEN})

    async def asyncTearDown(self):
        await self.client.aclose()
        await self.lifespan.__aexit__(None, None, None)

    async def test_project_session_settings_and_diff_endpoints(self):
        result = await self.client.post('/api/projects', json={'path': str(self.project)})
        self.assertEqual(result.status_code, 201, result.text)
        pid = result.json()['id']
        self.assertEqual(len((await self.client.get('/api/projects')).json()), 1)
        result = await self.client.post(f'/api/projects/{pid}/sessions', json={'player': 'codex', 'title': 'test'})
        self.assertEqual(result.status_code, 201, result.text)
        sid = result.json()['id']
        self.assertEqual(len((await self.client.get(f'/api/projects/{pid}/sessions')).json()), 1)
        self.assertEqual((await self.client.get(f'/api/sessions/{sid}/diff')).status_code, 200)
        result = await self.client.put('/api/settings', json={'permission': 'read-only'})
        self.assertEqual(result.json()['permission'], 'read-only')
        self.assertIs(result.json()['openrouter_key_present'], False)

    async def test_host_origin_token_and_forwarding_restrictions(self):
        for headers in ({'Authorization': ''}, {'Authorization': 'Bearer wrong'},
                        {'Host': 'evil.test'}, {'Host': f'localhost:{PORT}'},
                        {'Origin': 'https://evil.test'}, {'Origin': 'null'},
                        {'X-Forwarded-Host': f'127.0.0.1:{PORT}'}, {'Forwarded': 'host=evil.test'}):
            with self.subTest(headers=headers):
                result = await self.client.get('/api/projects', headers=headers)
                self.assertIn(result.status_code, {401, 403})
        result = await self.client.get('/api/projects', headers={'Origin': f'http://127.0.0.1:{PORT}'})
        self.assertEqual(result.status_code, 200)

    async def test_validation_errors_are_clean_and_secrets_are_not_settings(self):
        for path, body in [('/api/projects', {'path': '../missing'}),
                           ('/api/settings', {'api_key': 'do-not-store'}),
                           ('/api/settings', {'permission': []}),
                           ('/api/settings', {'default_player': {}})]:
            result = await self.client.request('PUT' if 'settings' in path else 'POST', path, json=body)
            self.assertEqual(result.status_code, 400, result.text)
            self.assertNotIn('do-not-store', result.text)
        self.assertEqual((await self.client.get('/api/projects/missing/sessions')).status_code, 404)
        result = await self.client.post('/api/projects', content='not-json', headers={'Content-Type': 'application/json'})
        self.assertEqual(result.status_code, 400)
        result = await self.client.post('/api/projects', json={'path': 'x' * 1100000})
        self.assertEqual(result.status_code, 413)

    async def test_git_diff_drains_large_output_and_reports_explicit_cap(self):
        from orchestra.app.server import git_read
        def git(*args):
            result = subprocess.run(['git', '-C', str(self.project), *args], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            return result.stdout.strip()
        git('init')
        source = self.project / 'source.txt'
        source.write_text('old\n', encoding='utf-8')
        git('add', 'source.txt')
        git('-c', 'user.name=Test', '-c', 'user.email=test@example.invalid', 'commit', '-m', 'fixture')
        commit = git('rev-parse', 'HEAD')
        source.write_text(''.join(f'line-{i:06d}\n' for i in range(20000)), encoding='utf-8')
        output, truncated = await git_read(self.project, 'diff', '--no-ext-diff', '--no-textconv', commit, '--')
        self.assertFalse(truncated)
        self.assertIn('+line-019999', output)
        output, truncated = await git_read(self.project, 'diff', '--no-ext-diff', '--no-textconv', commit, '--', limit=2000)
        self.assertTrue(truncated)
        self.assertLessEqual(len(output), 2000)
        hook = self.project / '.git' / 'fsmonitor-hook'
        hook.write_text('#!/bin/sh\nprintf bad > fsmonitor-executed.txt\nprintf "\\000"\n', encoding='utf-8', newline='\n')
        hook.chmod(0o700)
        git('config', 'core.fsmonitor', hook.as_posix())
        _ = await git_read(self.project, 'diff', '--no-ext-diff', '--no-textconv', commit, '--')
        self.assertFalse((self.project / 'fsmonitor-executed.txt').exists(), 'diff executed a repository hook')


class WebSocketSecurityTests(unittest.IsolatedAsyncioTestCase):
    async def test_unauthenticated_or_cross_origin_socket_is_rejected(self):
        from orchestra.app.security import LocalSecurity
        reached = []
        async def downstream(scope, receive, send):
            reached.append(True)
        middleware = LocalSecurity(downstream, port=PORT, token=TOKEN)
        for extra in ([], [(b'origin', b'https://evil.test'),
                          (b'sec-websocket-protocol', ('orchestra, orchestra.token.' + TOKEN).encode())]):
            sent = []
            async def send(message):
                sent.append(message)
            scope = {'type': 'websocket', 'headers': [(b'host', f'127.0.0.1:{PORT}'.encode()), *extra], 'subprotocols': []}
            await middleware(scope, None, send)
            self.assertEqual(sent[0]['type'], 'websocket.close')
        self.assertEqual(reached, [])


if __name__ == '__main__':
    unittest.main()

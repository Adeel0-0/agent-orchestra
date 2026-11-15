import asyncio
import tempfile
import unittest
from pathlib import Path

from test_app_server import HAS_APP, TOKEN, PORT
if HAS_APP:
    import httpx
from orchestra.app.store import Store


@unittest.skipUnless(HAS_APP, 'Install .[test-app] for UI API tests')
class BootstrapTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        from orchestra.app.server import create_app
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.web = root / 'dist'
        (self.web / 'assets').mkdir(parents=True)
        (self.web / 'index.html').write_text('<html><body>inert shell</body></html>')
        (self.web / 'assets' / 'app.js').write_text('console.log("static")')
        self.store = Store(root / 'data')
        self.addCleanup(self.store.close)
        self.app = create_app(store=self.store, port=PORT, token=TOKEN, web_dist=self.web,
                              folder_picker=lambda: str(root))
        self.lifespan = self.app.router.lifespan_context(self.app)
        await self.lifespan.__aenter__()
        self.client = httpx.AsyncClient(transport=httpx.ASGITransport(app=self.app), base_url=f'http://127.0.0.1:{PORT}')

    async def asyncTearDown(self):
        await self.client.aclose()
        await self.lifespan.__aexit__(None, None, None)

    async def test_static_shell_is_public_but_never_contains_launch_token(self):
        shell = await self.client.get('/')
        self.assertEqual(shell.status_code, 200)
        self.assertNotIn(TOKEN, shell.text)
        self.assertIn("default-src 'self'", shell.headers['content-security-policy'])
        self.assertEqual((await self.client.get('/assets/app.js')).status_code, 200)
        self.assertEqual((await self.client.get('/api/projects')).status_code, 401)
        self.assertEqual((await self.client.get('/', headers={'Host': 'evil.test'})).status_code, 403)
        self.assertEqual((await self.client.get('/assets/../orchestra.db')).status_code, 401)

    async def test_nonce_is_single_use_and_requires_exact_origin(self):
        nonce = self.app.state.bootstrap.issue()
        response = await self.client.post('/api/bootstrap', json={'nonce': nonce})
        self.assertEqual(response.status_code, 403)
        headers = {'Origin': f'http://127.0.0.1:{PORT}'}
        response = await self.client.post('/api/bootstrap', json={'nonce': nonce}, headers=headers)
        self.assertEqual(response.status_code, 200)
        self.assertEqual(response.json()['token'], TOKEN)
        response = await self.client.post('/api/bootstrap', json={'nonce': nonce}, headers=headers)
        self.assertEqual(response.status_code, 401)

    async def test_expired_nonce_and_query_token_cannot_authenticate(self):
        nonce = self.app.state.bootstrap.issue(ttl=-1)
        headers = {'Origin': f'http://127.0.0.1:{PORT}'}
        self.assertEqual((await self.client.post('/api/bootstrap', json={'nonce': nonce}, headers=headers)).status_code, 401)
        self.assertEqual((await self.client.get('/api/projects?token=' + TOKEN)).status_code, 401)
        self.assertEqual((await self.client.get('/api/bootstrap?nonce=x')).status_code, 401)

    async def test_native_picker_requires_api_auth(self):
        self.assertEqual((await self.client.post('/api/folders/pick', json={})).status_code, 401)
        response = await self.client.post('/api/folders/pick', json={}, headers={'Authorization': 'Bearer ' + TOKEN})
        self.assertEqual(response.status_code, 200)
        self.assertTrue(Path(response.json()['path']).is_dir())

    async def test_concurrent_nonce_exchange_has_exactly_one_winner(self):
        nonce = self.app.state.bootstrap.issue()
        headers = {'Origin': f'http://127.0.0.1:{PORT}'}
        responses = await asyncio.gather(*[
            self.client.post('/api/bootstrap', json={'nonce': nonce}, headers=headers)
            for _ in range(8)])
        self.assertEqual(sorted(r.status_code for r in responses), [200] + [401] * 7)

    async def test_foreign_origin_and_oversized_exchange_cannot_consume_nonce(self):
        nonce = self.app.state.bootstrap.issue()
        response = await self.client.post('/api/bootstrap', json={'nonce': nonce}, headers={'Origin': 'https://foreign.example'})
        self.assertEqual(response.status_code, 403)
        headers = {'Origin': f'http://127.0.0.1:{PORT}'}
        response = await self.client.post('/api/bootstrap', json={'nonce': nonce, 'padding': 'x' * 1200}, headers=headers)
        self.assertEqual(response.status_code, 413)
        response = await self.client.post('/api/bootstrap', json={'nonce': nonce}, headers=headers)
        self.assertEqual(response.status_code, 200)


if __name__ == '__main__':
    unittest.main()

"""Documented SSE examples and a local HTTP server; never use real keys."""
import asyncio
import io
import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch


class SseTests(unittest.TestCase):
    def parse(self, text):
        from orchestra.players.openrouter_client import parse_sse
        return list(parse_sse(io.BytesIO(text.encode())))

    def test_comments_multiline_usage_and_done(self):
        items = self.parse(': keepalive\r\ndata: {"choices":\r\ndata: [], "usage":{"cost":0.01}}\r\n\r\ndata: [DONE]\n\n')
        self.assertEqual(items, [{'choices': [], 'usage': {'cost': 0.01}}])

    def test_truncated_or_invalid_stream_is_error(self):
        from orchestra.players.openrouter_client import OpenRouterError
        for text in ('data: {bad}\n\n', 'data: {"choices":[]}\n\n', 'data: []\n\n'):
            with self.subTest(text=text), self.assertRaises(OpenRouterError):
                self.parse(text)

    def test_midstream_error_is_not_success(self):
        from orchestra.players.openrouter_client import OpenRouterError
        with self.assertRaisesRegex(OpenRouterError, 'Provider failed'):
            self.parse('data: {"error":{"message":"Provider failed"}}\n\ndata: [DONE]\n\n')


class ClientTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.requests = []
        requests = self.requests
        self.release = threading.Event()
        release = self.release

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self.send_response(200)
                self.end_headers()
                if self.path.startswith('/hang/'):
                    release.wait(5)
                self.wfile.write(json.dumps({'data': [
                    {'id': 'test/tools', 'supported_parameters': ['tools'], 'context_length': 8192},
                    {'id': 'test/plain', 'supported_parameters': []}]}).encode())

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                requests.append((self.path, self.headers.get('Authorization'), body))
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.end_headers()
                if body.get('model') == 'test/hang':
                    self.wfile.write(b'data: {"choices":[{"delta":{"content":"waiting"},"finish_reason":null}]}\n\n')
                    self.wfile.flush()
                    release.wait(5)
                self.wfile.write(b'data: {"choices":[{"delta":{"content":"pong"},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n')

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = f'http://127.0.0.1:{self.server.server_port}/api/v1'

    async def asyncTearDown(self):
        self.release.set()
        await asyncio.to_thread(self.server.shutdown)
        self.server.server_close()
        self.thread.join()

    async def test_catalog_filters_tools_and_stream_posts_contract(self):
        from orchestra.players.openrouter_client import OpenRouterClient
        client = OpenRouterClient(key='test-key', base_url=self.url)
        models = await client.models()
        self.assertEqual([m['id'] for m in models], ['test/tools'])
        chunks = [chunk async for chunk in client.stream({'model': 'test/tools', 'messages': []})]
        self.assertEqual(chunks[0]['choices'][0]['delta']['content'], 'pong')
        path, auth, body = self.requests[0]
        self.assertEqual(path, '/api/v1/chat/completions')
        self.assertEqual(auth, 'Bearer test-key')
        self.assertTrue(body['stream'])
        self.assertTrue(body['stream_options']['include_usage'])

    async def test_missing_key_fails_before_network(self):
        from orchestra.players.openrouter_client import OpenRouterClient, OpenRouterError
        client = OpenRouterClient(key='', base_url=self.url)
        with self.assertRaisesRegex(OpenRouterError, 'key'):
            _ = [c async for c in client.stream({'model': 'x', 'messages': []})]
        self.assertEqual(self.requests, [])

    async def test_external_cancel_stops_consumption_during_idle_read(self):
        from orchestra.players.openrouter_client import OpenRouterClient
        client = OpenRouterClient(key='test-key', base_url=self.url)
        started = asyncio.Event()

        async def consume():
            async for _ in client.stream({'model': 'test/hang', 'messages': []}):
                started.set()

        task = asyncio.create_task(consume())
        await asyncio.wait_for(started.wait(), 2)
        await client.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await asyncio.wait_for(task, 0.5)

    async def test_cancel_during_model_catalog_does_not_wait_for_default_executor(self):
        from orchestra.players.openrouter_client import OpenRouterClient
        client = OpenRouterClient(key='test-key', base_url=self.url.replace('/api/v1', '/hang'))
        task = asyncio.create_task(client.models())
        await asyncio.sleep(0.05)
        await client.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await asyncio.wait_for(task, 0.5)


class CredentialTests(unittest.TestCase):
    def test_credentials_only_use_secure_keyring(self):
        from orchestra.players.credentials import get_key, set_key, CredentialError
        # External OS credential API only. Never touch the real user's credential.
        class MemoryVault:
            priority = 1
            __module__ = 'keyring.backends.Windows'
        import types
        stored = {}
        keyring = types.SimpleNamespace(
            get_keyring=lambda: MemoryVault(),
            get_password=lambda service, user: stored.get((service, user)),
            set_password=lambda service, user, value: stored.__setitem__((service, user), value))
        with patch.dict('sys.modules', {'keyring': keyring}):
            set_key('test-secret')
            self.assertEqual(get_key(), 'test-secret')
            MemoryVault.__module__ = 'keyrings.alt.file'
            with self.assertRaises(CredentialError):
                set_key('test-secret')


if __name__ == '__main__':
    unittest.main()

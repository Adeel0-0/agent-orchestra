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
        self.gets = []
        requests = self.requests
        gets = self.gets
        self.release = threading.Event()
        release = self.release

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                gets.append(self.path)
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
                if body.get('model') in {'test/http-error', 'test/retry', 'test/retry-wait'} and (
                    body['model'] != 'test/retry' or sum(r[2]['model'] == 'test/retry' for r in requests) == 1):
                    self.send_response(400 if body['model'] == 'test/http-error' else 429)
                    self.send_header('Retry-After', '10' if body['model'] == 'test/retry-wait' else '0')
                    self.end_headers()
                    self.wfile.write(b'{"error":{"message":"Unsupported parameter for test-key"}}')
                    return
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.end_headers()
                if body.get('model') == 'test/partial-error':
                    self.wfile.write(b'data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}\n\ndata: {"error":{"code":503,"message":"unavailable"}}\n\n')
                    return
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

    async def test_http_error_explains_reason_and_redacts_actual_key(self):
        from orchestra.players.openrouter_client import OpenRouterClient, OpenRouterError
        with self.assertRaises(OpenRouterError) as caught:
            _ = [c async for c in OpenRouterClient(key='test-key', base_url=self.url).stream({'model': 'test/http-error'})]
        self.assertIn('Unsupported parameter', str(caught.exception))
        self.assertNotIn('test-key', str(caught.exception))
        self.assertEqual(len(self.requests), 1)

    async def test_retries_transient_http_only_before_any_streamed_chunk(self):
        from orchestra.players.openrouter_client import OpenRouterClient, OpenRouterError
        client = OpenRouterClient(key='test-key', base_url=self.url, retry_backoff=0)
        chunks = [c async for c in client.stream({'model': 'test/retry'})]
        self.assertEqual(chunks[0]['choices'][0]['delta']['content'], 'pong')
        self.assertEqual(len(self.requests), 2)
        with self.assertRaises(OpenRouterError):
            _ = [c async for c in client.stream({'model': 'test/partial-error'})]
        self.assertEqual(len(self.requests), 3)

    async def test_catalog_is_cached_across_new_clients(self):
        from orchestra.players.openrouter_client import OpenRouterClient
        first = await OpenRouterClient(base_url=self.url).models()
        first[0]['id'] = 'mutated-by-caller'
        second = await OpenRouterClient(base_url=self.url).models()
        self.assertEqual(second[0]['id'], 'test/tools')
        self.assertEqual(len(self.gets), 1)

    async def test_cancel_during_retry_backoff_prevents_second_request(self):
        from orchestra.players.openrouter_client import OpenRouterClient
        client = OpenRouterClient(key='test-key', base_url=self.url)
        async def consume():
            return [c async for c in client.stream({'model': 'test/retry-wait'})]
        task = asyncio.create_task(consume())
        async def backoff_started():
            while client._retry_wait is None:
                await asyncio.sleep(.01)
        await asyncio.wait_for(backoff_started(), 3)
        await client.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await asyncio.wait_for(task, .5)
        self.assertEqual(len(self.requests), 1)

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

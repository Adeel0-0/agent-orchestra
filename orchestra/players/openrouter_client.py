"""OpenRouter's public Chat Completions API over stdlib urllib and SSE.

Docs: https://openrouter.ai/docs/api/reference/streaming
      https://openrouter.ai/docs/guides/features/tool-calling
No subscription tokens, private endpoints, automatic retries or paid model defaults.
"""
import asyncio
import json
import queue
import threading
import urllib.error
import urllib.request


class OpenRouterError(RuntimeError):
    pass


def parse_sse(response):
    """Parse SSE frames, not transport chunks. Require the explicit DONE sentinel."""
    data, size = [], 0
    while True:
        raw = response.readline(2 * 1024 * 1024 + 1)
        if not raw:
            raise OpenRouterError('OpenRouter stream ended before [DONE].')
        size += len(raw)
        if size > 2 * 1024 * 1024:
            raise OpenRouterError('OpenRouter SSE event exceeds 2 MiB.')
        try:
            line = raw.decode('utf-8').rstrip('\r\n')
        except UnicodeError:
            raise OpenRouterError('Invalid UTF-8 in OpenRouter stream.') from None
        if line:
            if line.startswith('data:'):
                value = line[5:]
                data.append(value[1:] if value.startswith(' ') else value)
            continue
        payload = '\n'.join(data)
        data, size = [], 0
        if not payload:
            continue
        if payload == '[DONE]':
            return
        try:
            obj = json.loads(payload)
        except ValueError:
            raise OpenRouterError('Invalid JSON in OpenRouter stream.') from None
        if not isinstance(obj, dict):
            raise OpenRouterError('Expected an object in OpenRouter stream.')
        if obj.get('error'):
            error = obj['error']
            message = error.get('message', 'OpenRouter stream failed') if isinstance(error, dict) else str(error)
            raise OpenRouterError(str(message))
        yield obj


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # Never forward Authorization to a redirected host.
        raise OpenRouterError('OpenRouter redirect refused.')


class OpenRouterClient:
    def __init__(self, key=None, base_url='https://openrouter.ai/api/v1', timeout=30):
        self._key = key
        self.base_url = base_url.rstrip('/')
        self.timeout = timeout
        self._active = None

    def _get_key(self):
        if self._key is None:
            from .credentials import get_key
            self._key = get_key()
        if not self._key:
            raise OpenRouterError('OpenRouter API key is missing. Run: orchestra openrouter-key')
        return self._key

    def _open(self, endpoint, body=None):
        headers = {'Accept': 'text/event-stream' if body is not None else 'application/json'}
        if body is not None:
            headers.update({'Authorization': 'Bearer ' + self._get_key(), 'Content-Type': 'application/json'})
        req = urllib.request.Request(self.base_url + endpoint,
                                     data=json.dumps(body).encode() if body is not None else None,
                                     headers=headers)
        try:
            return urllib.request.build_opener(_NoRedirect()).open(req, timeout=self.timeout)
        except urllib.error.HTTPError as exc:
            # Don't echo response bodies: a remote provider might include request secrets.
            code = exc.code
            exc.close()
            raise OpenRouterError(f'OpenRouter HTTP {code}. Check credentials, credit, model access or rate limits.') from None
        except (urllib.error.URLError, OSError):
            raise OpenRouterError('Cannot connect to OpenRouter (network error or timeout).') from None

    async def models(self):
        def load(state):
            with self._open('/models') as response:
                state['response'] = response
                data = response.read(8 * 1024 * 1024 + 1)
                if len(data) > 8 * 1024 * 1024:
                    raise OpenRouterError('Model catalog is too large.')
                try:
                    models = json.loads(data)['data']
                    yield [m for m in models if isinstance(m, dict) and isinstance(m.get('id'), str)
                            and 'tools' in (m.get('supported_parameters') or [])]
                except (ValueError, KeyError, TypeError):
                    raise OpenRouterError('Invalid OpenRouter model catalog.') from None
        bridge = self._bridge(load)
        try:
            async for models in bridge:
                return models
        finally:
            await bridge.aclose()

    async def stream(self, body):
        self._get_key()

        def read(state):
            with self._open('/chat/completions', {**body, 'stream': True, 'stream_options': {'include_usage': True}}) as response:
                state['response'] = response
                yield from parse_sse(response)

        bridge = self._bridge(read)
        try:
            async for chunk in bridge:
                yield chunk
        finally:
            await bridge.aclose()

    async def _bridge(self, read):
        # urllib blocks. A bounded bridge keeps the event loop free for approvals/cancel.
        # Daemon worker cannot hold up shutdown; idle network reads time out after 30s.
        if self._active is not None:
            raise OpenRouterError('A request is already running.')
        stopped = threading.Event()
        items = queue.Queue(maxsize=16)
        state = {'response': None}
        self._active = (stopped, state)

        def put(item):
            while not stopped.is_set():
                try:
                    items.put(item, timeout=0.1)
                    return
                except queue.Full:
                    continue

        def worker():
            try:
                if stopped.is_set():
                    return
                for chunk in read(state):
                    if stopped.is_set():
                        break
                    put(chunk)
            except Exception as exc:
                message = str(exc).replace(self._key, '[redacted]') if self._key else str(exc)
                put(OpenRouterError(message if isinstance(exc, OpenRouterError) else 'OpenRouter stream failed (network or decoding error).'))
            finally:
                put(None)

        threading.Thread(target=worker, name='orchestra-openrouter', daemon=True).start()
        try:
            while True:
                if stopped.is_set():
                    raise asyncio.CancelledError()
                try:
                    item = items.get_nowait()
                except queue.Empty:
                    await asyncio.sleep(0.01)
                    continue
                if item is None:
                    return
                if isinstance(item, Exception):
                    raise item
                yield item
        finally:
            stopped.set()
            response = state['response']
            if response is not None:
                # HTTPResponse.close may wait on a blocked reader; never block asyncio.
                threading.Thread(target=response.close, daemon=True).start()
            self._active = None

    async def cancel(self):
        if self._active:
            stopped, state = self._active
            stopped.set()
            if state['response'] is not None:
                threading.Thread(target=state['response'].close, daemon=True).start()

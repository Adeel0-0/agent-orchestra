"""One-use browser bootstrap and inert bundled assets. No API token in HTML."""
import secrets
import threading
import time
from pathlib import Path

from starlette.responses import FileResponse, JSONResponse
from starlette.routing import Mount, Route
from starlette.staticfiles import StaticFiles


class BootstrapTokens:
    def __init__(self):
        self._nonces = {}
        self._lock = threading.Lock()

    def issue(self, ttl=60):
        with self._lock:
            now = time.monotonic()
            self._nonces = {key: expiry for key, expiry in self._nonces.items() if expiry > now}
            if len(self._nonces) >= 16:
                self._nonces.pop(next(iter(self._nonces)))
            nonce = secrets.token_urlsafe(32)
            self._nonces[nonce] = now + min(ttl, 60)
            return nonce

    def consume(self, nonce):
        if not isinstance(nonce, str) or not 24 <= len(nonce) <= 128:
            return False
        with self._lock:
            expiry = self._nonces.pop(nonce, 0)
            return expiry > time.monotonic()


def bundled_web():
    return Path(__file__).parent / 'web' / 'dist'


def public_ui_request(scope):
    if scope['type'] != 'http':
        return False
    path, method = scope.get('path'), scope.get('method')
    return (method in {'GET', 'HEAD'} and (path == '/' or path.startswith('/assets/'))) or (path == '/api/bootstrap' and method == 'POST')


def ui_routes(root, bootstrap, token, port, read_body):
    root = Path(root).resolve()
    if not (root / 'index.html').is_file():
        raise RuntimeError('UI assets are missing. Build web/ with npm ci and npm run build, or install a packaged wheel.')

    async def shell(request):
        return FileResponse(root / 'index.html', media_type='text/html')

    async def exchange(request):
        if request.headers.get('origin') != f'http://127.0.0.1:{port}':
            return JSONResponse({'error': 'Local browser Origin required'}, status_code=403)
        data = await read_body(request, {'nonce'}, max_size=1024)
        if not bootstrap.consume(data.get('nonce')):
            return JSONResponse({'error': 'Bootstrap expired or already used. Reopen Orchestra from the launcher.'}, status_code=401)
        return JSONResponse({'token': token})

    return [Route('/', shell), Route('/api/bootstrap', exchange, methods=['POST']),
            Mount('/assets', StaticFiles(directory=root / 'assets'), name='assets')]

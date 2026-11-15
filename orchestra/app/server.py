"""Phase 3 local API; no UI and no agent work tied to request lifetime."""
import asyncio
import json
import secrets
from contextlib import asynccontextmanager

from starlette.applications import Starlette
from starlette.middleware import Middleware
from starlette.requests import Request
from starlette.responses import JSONResponse
from starlette.routing import Route, WebSocketRoute
from starlette.websockets import WebSocketDisconnect

from .security import LocalSecurity
from .sessions import SessionManager, Conflict, SlowSubscriber
from .store import Store
from .errors import RequestError, NotFound


class BodyTooLarge(RequestError):
    pass


async def body(request, allowed, max_size=1024 * 1024):
    if request.headers.get('content-type', '').split(';')[0] != 'application/json':
        raise RequestError('Expected application/json')
    chunks, size = [], 0
    async for chunk in request.stream():
        size += len(chunk)
        if size > max_size:
            raise BodyTooLarge(f'Request body exceeds {max_size} bytes')
        chunks.append(chunk)
    try:
        data = json.loads(b''.join(chunks))
    except (ValueError, UnicodeError):
        raise RequestError('Invalid JSON body') from None
    if not isinstance(data, dict) or set(data) - set(allowed):
        raise RequestError('Unexpected request fields')
    return data


async def git_read(cwd, *args, limit=1024 * 1024):
    try:
        proc = await asyncio.create_subprocess_exec('git', '-c', 'core.fsmonitor=false', '--no-pager', '-C', str(cwd), *args,
            stdin=asyncio.subprocess.DEVNULL, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL)
    except FileNotFoundError:
        return None, False
    try:
        async def read():
            output = bytearray()
            while True:
                chunk = await proc.stdout.read(min(65536, limit + 1 - len(output)))
                if not chunk:
                    break
                output.extend(chunk)
                if len(output) > limit:
                    return bytes(output[:limit]), True
            await proc.wait()
            return bytes(output) if proc.returncode == 0 else None, False
        output, truncated = await asyncio.wait_for(read(), timeout=10)
        return output.decode('utf-8', errors='replace') if output is not None else None, truncated
    except asyncio.TimeoutError:
        raise RequestError('Git inspection timed out') from None
    finally:
        if proc.returncode is None:
            proc.kill()
        # Drain already buffered pipe data after killing; wait() alone can deadlock
        # when asyncio paused a full pipe before the child exited.
        await proc.communicate()


def create_app(*, store=None, port, token=None, factory=None, web_dist=None, folder_picker=None):
    if not isinstance(port, int) or not 1 <= port <= 65535:
        raise RequestError('Expected a bound loopback port')
    token = token or secrets.token_urlsafe(32)
    if not isinstance(token, str) or not token.isascii() or len(token) < 24:
        raise RequestError('Launch token must contain at least 24 ASCII characters')
    store = store if store is not None else Store()
    manager = SessionManager(store, **({'factory': factory} if factory is not None else {}))
    from .ui import BootstrapTokens, public_ui_request, ui_routes
    bootstrap = BootstrapTokens()

    @asynccontextmanager
    async def lifespan(app):
        try:
            yield
        finally:
            await manager.close()
            store.close()

    async def error(request, exc):
        status = 413 if isinstance(exc, BodyTooLarge) else 404 if isinstance(exc, NotFound) else 409 if isinstance(exc, Conflict) else 400
        return JSONResponse({'error': exc.args[0] if exc.args else 'Invalid request'}, status_code=status)

    async def projects(request):
        if request.method == 'GET':
            return JSONResponse(store.projects())
        data = await body(request, {'path'})
        return JSONResponse(store.add_project(data.get('path')), status_code=201)

    async def sessions(request):
        pid = request.path_params['pid']
        if request.method == 'GET':
            return JSONResponse(store.sessions(pid))
        data = await body(request, {'player', 'model', 'permission', 'title'})
        defaults = store.settings()
        project = store.project(pid)
        base_commit, _ = await git_read(project['path'], 'rev-parse', '--verify', 'HEAD', limit=128)
        session = store.create_session(pid, data.get('player', defaults['default_player']), data.get('model'),
            data.get('permission', defaults['permission']), data.get('title', 'New session'),
            base_commit=base_commit.strip() if base_commit else None)
        return JSONResponse(session, status_code=201)

    async def messages(request):
        data = await body(request, {'prompt'})
        sid = request.path_params['sid']
        manager.start(sid, data.get('prompt'))
        if store.session(sid)['title'] == 'New session':
            store.update_session(sid, title=data['prompt'].strip().splitlines()[0][:60])
        return JSONResponse({'session_id': sid, 'status': 'running'}, status_code=202)

    async def capabilities(request):
        return JSONResponse({'native_folder_picker': folder_picker is not None})

    async def pick_folder(request):
        await body(request, set())
        if folder_picker is None:
            raise RequestError('Native folder picker is unavailable; enter the folder path.')
        path = await asyncio.to_thread(folder_picker)
        return JSONResponse({'path': str(path) if path else None})

    async def cancel(request):
        data = await body(request, {'confirm_stopped'}) if request.headers.get('content-type') else {}
        await manager.cancel(request.path_params['sid'], confirm_stopped=data.get('confirm_stopped', False))
        return JSONResponse({'status': store.session(request.path_params['sid'])['status']})

    async def permission(request):
        data = await body(request, {'allow'})
        await manager.answer(request.path_params['sid'], request.path_params['rid'], data.get('allow'))
        return JSONResponse({'ok': True})

    async def settings(request):
        if request.method == 'PUT':
            store.set_settings(await body(request, {'default_player', 'permission'}))
        from ..players.credentials import get_key, CredentialError
        def key_present():
            try:
                return bool(get_key())
            except CredentialError:
                return False
        return JSONResponse({**store.settings(), 'openrouter_key_present': await asyncio.to_thread(key_present)})

    async def players(request):
        from dataclasses import asdict
        from ..players.registry import check_all
        from ..players.openrouter_client import OpenRouterClient, OpenRouterError
        health = await asyncio.to_thread(check_all)
        catalog, catalog_error = [], None
        client = OpenRouterClient()
        try:
            catalog = await client.models()
        except OpenRouterError as exc:
            catalog_error = str(exc)
        return JSONResponse([{**asdict(h), 'ready': h.ready, 'models': catalog if h.id == 'openrouter' else [],
            'models_error': catalog_error if h.id == 'openrouter' else None} for h in health])

    async def diff(request):
        session = store.session(request.path_params['sid'])
        if not session['base_commit']:
            return JSONResponse({'base_commit': None, 'diff': '', 'untracked': [], 'truncated': False})
        cwd = session['worktree'] or store.project(session['project_id'])['path']
        output, truncated = await git_read(cwd, 'diff', '--no-ext-diff', '--no-textconv', session['base_commit'], '--')
        untracked, more = await git_read(cwd, 'ls-files', '--others', '--exclude-standard', '-z')
        if output is None:
            raise RequestError('Git diff is unavailable; the saved base may no longer exist')
        return JSONResponse({'base_commit': session['base_commit'], 'diff': output,
            'untracked': [p for p in (untracked or '').split('\x00') if p], 'truncated': truncated or more})

    async def events(websocket):
        sid = websocket.path_params['sid']
        try:
            after = int(websocket.query_params.get('after', '0'))
            if not 0 <= after <= store.last_seq(sid):
                raise RequestError('Invalid replay offset')
        except (KeyError, ValueError):
            await websocket.close(code=1008)
            return
        protocol = 'orchestra' if 'orchestra' in websocket.scope.get('subprotocols', []) else None
        await websocket.accept(subprotocol=protocol)
        stream = manager.events(sid, after)

        async def forward():
            async for event in stream:
                await websocket.send_json(event)

        async def disconnected():
            while True:
                message = await websocket.receive()
                if message['type'] == 'websocket.disconnect':
                    return
        sending, receiving = asyncio.create_task(forward()), asyncio.create_task(disconnected())
        try:
            done, _ = await asyncio.wait({sending, receiving}, return_when=asyncio.FIRST_COMPLETED)
            for task in done:
                await task
        except SlowSubscriber:
            await websocket.close(code=1013, reason='Reconnect with last received seq')
        except (WebSocketDisconnect, OSError, RuntimeError):
            pass
        finally:
            sending.cancel()
            receiving.cancel()
            await asyncio.gather(sending, receiving, return_exceptions=True)
            await stream.aclose()

    routes = [
        Route('/api/capabilities', capabilities), Route('/api/folders/pick', pick_folder, methods=['POST']),
        Route('/api/players', players), Route('/api/projects', projects, methods=['GET', 'POST']),
        Route('/api/projects/{pid}/sessions', sessions, methods=['GET', 'POST']),
        Route('/api/sessions/{sid}/messages', messages, methods=['POST']),
        Route('/api/sessions/{sid}/cancel', cancel, methods=['POST']),
        Route('/api/sessions/{sid}/permissions/{rid}', permission, methods=['POST']),
        Route('/api/sessions/{sid}/diff', diff), Route('/api/settings', settings, methods=['GET', 'PUT']),
        WebSocketRoute('/api/sessions/{sid}/events', events)]
    if web_dist is not None:
        routes += ui_routes(web_dist, bootstrap, token, port, body)
    app = Starlette(routes=routes,
        middleware=[Middleware(LocalSecurity, port=port, token=token, public=public_ui_request if web_dist is not None else None)],
        exception_handlers={RequestError: error, NotFound: error, Conflict: error}, lifespan=lifespan)
    app.state.store, app.state.manager, app.state.token = store, manager, token
    app.state.bootstrap = bootstrap
    return app

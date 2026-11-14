"""Start the Phase 3 API on a reserved random IPv4 loopback port."""
import os
import secrets
import socket


def reserve_socket():
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        if os.name == 'nt':
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
        sock.bind(('127.0.0.1', 0))
        sock.listen(128)
        sock.setblocking(False)
        return sock
    except BaseException:
        sock.close()
        raise


def serve(data_dir=None):
    try:
        import uvicorn
        from .server import create_app
    except ImportError:
        raise RuntimeError('Install the local API extra: python -m pip install -e ".[app]"') from None
    from .store import Store
    with reserve_socket() as sock:
        port = sock.getsockname()[1]
        token = secrets.token_urlsafe(32)
        store = Store(data_dir)
        try:
            app = create_app(store=store, port=port, token=token)
            print(f'Orchestra API: http://127.0.0.1:{port}', flush=True)
            print(f'Authorization: Bearer {token}', flush=True)
            print('Keep this launch token private. Ctrl+C stops the server. No UI is served yet.', flush=True)
            server = uvicorn.Server(uvicorn.Config(app, host='127.0.0.1', port=port,
                proxy_headers=False, server_header=False, access_log=False, log_level='warning',
                timeout_graceful_shutdown=5))
            server.run(sockets=[sock])
        finally:
            store.close()
    return 0

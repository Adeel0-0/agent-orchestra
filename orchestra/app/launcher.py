"""Start the Phase 3 API on a reserved random IPv4 loopback port."""
import os
import secrets
import socket
import threading
import time
import webbrowser


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
            print('Keep this launch token private. Ctrl+C stops the server. This command serves the API only.', flush=True)
            server = uvicorn.Server(uvicorn.Config(app, host='127.0.0.1', port=port,
                proxy_headers=False, server_header=False, access_log=False, log_level='warning',
                timeout_graceful_shutdown=5))
            server.run(sockets=[sock])
        finally:
            store.close()
    return 0


class AppRuntime:
    """Background API owned by one native window or browser-mode terminal."""
    def __init__(self, data_dir=None, web_dist=None, folder_picker=None):
        import uvicorn
        from .server import create_app
        from .ui import bundled_web
        from .store import Store
        self.sock = reserve_socket()
        self.port = self.sock.getsockname()[1]
        self.origin = f'http://127.0.0.1:{self.port}'
        store = None
        try:
            store = Store(data_dir)
            self.app = create_app(store=store, port=self.port, web_dist=web_dist or bundled_web(), folder_picker=folder_picker)
        except BaseException:
            if store is not None:
                store.close()
            self.sock.close()
            raise
        self.server = uvicorn.Server(uvicorn.Config(self.app, proxy_headers=False, server_header=False,
            access_log=False, log_level='warning', timeout_graceful_shutdown=5))
        self._failure = None

        def run():
            try:
                self.server.run(sockets=[self.sock])
            except BaseException as exc:
                self._failure = exc
            finally:
                self.app.state.store.close()
        # This process owns agent cleanup even after the last window closes.
        # A slow shutdown must not vanish when the foreground launcher returns.
        self.thread = threading.Thread(target=run, name='orchestra-ui-server', daemon=False)

    def start(self):
        self.thread.start()
        until = time.monotonic() + 10
        while not self.server.started:
            if not self.thread.is_alive() or time.monotonic() > until:
                self.stop()
                raise RuntimeError('Local UI server could not start') from self._failure
            time.sleep(.02)
        return self

    def bootstrap_url(self):
        return self.origin + '/#bootstrap=' + self.app.state.bootstrap.issue()

    def stop(self):
        self.server.should_exit = True
        if self.thread.ident is not None and threading.current_thread() is not self.thread:
            self.thread.join(timeout=10)
        if self.thread.is_alive():
            raise RuntimeError('Local server is still stopping; keep this process open until agent cleanup finishes')
        self.sock.close()
        self.app.state.store.close()


def launch_ui(data_dir=None, browser=False):
    try:
        import uvicorn  # noqa: F401 — fail before creating local data.
        if not browser:
            try:
                import webview
            except ImportError:
                print('Native window support is not installed; opening your browser. Install .[desktop] for a native window.')
                browser = True
    except ImportError:
        raise RuntimeError('Install the app extra: python -m pip install -e ".[app]"') from None
    holder = {}

    def pick_folder():
        selected = holder['window'].create_file_dialog(webview.FileDialog.FOLDER)
        return selected[0] if selected else None

    runtime = AppRuntime(data_dir=data_dir, folder_picker=None if browser else pick_folder)
    runtime.start()
    try:
        url = runtime.bootstrap_url()
        if browser:
            webbrowser.open(url)
            print('Orchestra is open in your browser. Ctrl+C stops the local server.')
            while runtime.thread.is_alive():
                runtime.thread.join(.5)
        else:
            holder['window'] = webview.create_window('Agent Orchestra', url, width=1440, height=940,
                                                    min_size=(760, 600), background_color='#f2f5f9')
            webview.start(private_mode=True, debug=False)
    finally:
        runtime.stop()
    return 0

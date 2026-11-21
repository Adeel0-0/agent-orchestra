import json
import tempfile
import unittest
import urllib.request
from pathlib import Path
from unittest.mock import patch

from test_app_server import HAS_APP


class UiCommandTests(unittest.TestCase):
    def test_no_args_opens_ui_without_starting_engine_daemon(self):
        from orchestra.cli import main
        with patch('orchestra.app.launcher.launch_ui', return_value=0, create=True) as launch:
            self.assertEqual(main([]), 0)
        launch.assert_called_once()


@unittest.skipUnless(HAS_APP, 'Install .[test-app] for runtime checks')
class UiRuntimeTests(unittest.TestCase):
    def test_runtime_owns_server_and_nonce_never_contains_api_token(self):
        from orchestra.app.launcher import AppRuntime
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            web = root / 'web'
            (web / 'assets').mkdir(parents=True)
            (web / 'index.html').write_text('inert shell')
            runtime = AppRuntime(data_dir=root / 'data', web_dist=web)
            self.assertFalse(runtime.thread.daemon, 'Agent cleanup must outlive the foreground window')
            runtime.start()
            try:
                url = runtime.bootstrap_url()
                self.assertNotIn(runtime.app.state.token, url)
                nonce = url.split('#bootstrap=')[1]
                request = urllib.request.Request(runtime.origin + '/api/bootstrap', data=json.dumps({'nonce':nonce}).encode(),
                    headers={'Content-Type':'application/json', 'Origin':runtime.origin})
                with urllib.request.urlopen(request, timeout=3) as response:
                    self.assertEqual(json.load(response)['token'], runtime.app.state.token)
            finally:
                runtime.stop()
            self.assertFalse(runtime.thread.is_alive())


if __name__ == '__main__':
    unittest.main()

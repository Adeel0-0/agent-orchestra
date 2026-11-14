import contextlib
import io
import unittest


class LauncherTests(unittest.TestCase):
    def test_socket_is_random_port_and_ipv4_loopback(self):
        from orchestra.app.launcher import reserve_socket
        with reserve_socket() as sock:
            host, port = sock.getsockname()
            self.assertEqual(host, '127.0.0.1')
            self.assertGreater(port, 0)

    def test_serve_command_help_does_not_launch_or_require_gui(self):
        from orchestra.cli import main
        output = io.StringIO()
        with contextlib.redirect_stdout(output), self.assertRaises(SystemExit) as result:
            main(['serve', '--help'])
        self.assertEqual(result.exception.code, 0)
        self.assertIn('--data-dir', output.getvalue())


if __name__ == '__main__':
    unittest.main()

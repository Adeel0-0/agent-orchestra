"""Windows bootstrap: don't spawn a shell until parent establishes job ownership."""
import json
import subprocess
import sys


def main():
    raw = sys.stdin.buffer.readline(1024 * 1024 + 1)
    if not raw or len(raw) > 1024 * 1024:
        return 2
    command = json.loads(raw)
    if not isinstance(command, str):
        return 2
    return subprocess.run(['powershell.exe', '-NoProfile', '-NonInteractive', '-Command', command],
                          stdin=subprocess.DEVNULL).returncode


if __name__ == '__main__':
    sys.exit(main())

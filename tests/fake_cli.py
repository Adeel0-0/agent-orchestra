"""
A stand-in agent CLI for tests.

  fake_cli.py replay <fixture.jsonl> [exit_code]   print the fixture's lines, then exit
  fake_cli.py echo                                  print stdin back as one JSON line
  fake_cli.py hang <pidfile>                        start a grandchild, record its pid, sleep
"""

import json
import subprocess
import sys
import time

mode = sys.argv[1]

if mode == "replay":
    with open(sys.argv[2], encoding="utf-8") as f:
        for line in f:
            sys.stdout.write(line)
            sys.stdout.flush()
    sys.stderr.write("fake stderr line\n")
    sys.exit(int(sys.argv[3]) if len(sys.argv) > 3 else 0)

if mode == "echo":
    data = sys.stdin.read()
    print(json.dumps({"stdin": data}))
    sys.exit(0)

if mode == "hang":
    child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(120)"])
    with open(sys.argv[2], "w") as f:
        f.write(str(child.pid))
    print(json.dumps({"type": "thread.started", "thread_id": "hang"}), flush=True)
    time.sleep(120)

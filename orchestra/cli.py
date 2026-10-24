"""
Command Line Interface (CLI) for Agent Orchestra.
Usage:
    python -m orchestra status
    python -m orchestra claim TASK-001 --agent claude --scope "auth refactor"
    python -m orchestra release --agent claude
    python -m orchestra emit CUSTOM_EVENT '{"key": "value"}'
    python -m orchestra listen
    python -m orchestra start
    python -m orchestra run scripts/verifier.py
"""

from __future__ import annotations
import argparse
import json
import sys
from pathlib import Path
from typing import Optional

from .bus import EventBus, LockManager
from .config import OrchestraConfig, load_config
from .daemon import OrchestraDaemon
from .models import EventType
from .runner import ScriptRunner


def cmd_status(args: argparse.Namespace) -> int:
    cfg = load_config()
    daemon = OrchestraDaemon(cfg)
    status = daemon.get_status()

    if args.json:
        print(json.dumps(status, indent=2))
        return 0

    print("=== AGENT ORCHESTRA STATUS ===")
    print(f"State:        {status['state']}")
    print(f"Is Locked:    {status['is_locked']}")
    if status.get("active_lock"):
        l = status["active_lock"]
        print(f"  Task:       {l.get('task_id')}")
        print(f"  Agent:      {l.get('agent_id')}")
        print(f"  Scope:      {l.get('scope')}")
    if status.get("git"):
        g = status["git"]
        print(f"Git Branch:   {g.get('branch')} (HEAD: {g.get('head_sha')})")
        print(f"Git Clean:    {g.get('is_clean')} ({g.get('changed_count')} changed files)")
    if status.get("latest_event"):
        e = status["latest_event"]
        print(f"Latest Event: {e.get('event_type')} by {e.get('source_agent')}")
    return 0


def cmd_claim(args: argparse.Namespace) -> int:
    cfg = load_config()
    locks = LockManager(Path(cfg.workspace_root) / cfg.orchestra_dir)
    bus = EventBus(Path(cfg.workspace_root) / cfg.orchestra_dir)

    success = locks.acquire(
        task_id=args.task_id,
        agent_id=args.agent,
        scope=args.scope or "",
        lease_seconds=args.lease,
        force=args.force,
    )

    if not success:
        current = locks.get_current_lock()
        print(
            f"[FAILED] Cannot acquire lock: held by agent '{current.get('agent_id') if current else 'unknown'}' for task '{current.get('task_id') if current else 'unknown'}'. Use --force to override.",
            file=sys.stderr,
        )
        return 1

    bus.emit(
        event_type=EventType.AGENT_CLAIM,
        source_agent=args.agent,
        payload={"task_id": args.task_id, "scope": args.scope or ""},
    )
    print(f"[OK] Agent '{args.agent}' claimed writer lock for task '{args.task_id}'.")
    return 0


def cmd_release(args: argparse.Namespace) -> int:
    cfg = load_config()
    locks = LockManager(Path(cfg.workspace_root) / cfg.orchestra_dir)
    bus = EventBus(Path(cfg.workspace_root) / cfg.orchestra_dir)

    current = locks.get_current_lock()
    holding_agent = current.get("agent_id") if current else args.agent
    task_id = current.get("task_id") if current else "unknown"

    success = locks.release(agent_id=args.agent, force=args.force)
    if not success:
        print(
            f"[FAILED] Cannot release lock: held by '{holding_agent}', not '{args.agent}'. Use --force to override.",
            file=sys.stderr,
        )
        return 1

    bus.emit(
        event_type=EventType.HANDOFF_REQUEST,
        source_agent=args.agent or "unknown",
        payload={
            "task_id": task_id,
            "status": args.status,
            "summary": args.summary or "",
            "released_lock": current,
        },
    )
    print(f"[OK] Writer lock released for task '{task_id}'. Event emitted.")
    return 0


def cmd_emit(args: argparse.Namespace) -> int:
    cfg = load_config()
    bus = EventBus(Path(cfg.workspace_root) / cfg.orchestra_dir)

    payload = {}
    if args.payload:
        try:
            payload = json.loads(args.payload)
        except Exception as exc:
            print(f"[FAILED] Invalid JSON payload: {exc}", file=sys.stderr)
            return 1

    evt = bus.emit(
        event_type=args.event_type,
        source_agent=args.agent,
        payload=payload,
    )
    print(f"[OK] Emitted event {evt.event_id} ({evt.event_type.value})")
    return 0


def cmd_listen(args: argparse.Namespace) -> int:
    cfg = load_config()
    bus = EventBus(Path(cfg.workspace_root) / cfg.orchestra_dir)

    print("[LISTEN] Listening to Agent Orchestra event bus (Ctrl+C to stop)...")
    try:
        for evt in bus.stream_events(start_from_beginning=args.all):
            print(f"[{evt.event_type.value}] ({evt.source_agent}) -> {json.dumps(evt.payload)}")
    except KeyboardInterrupt:
        print("\nStopped.")
    return 0


def cmd_run(args: argparse.Namespace) -> int:
    cfg = load_config()
    runner = ScriptRunner(default_cwd=cfg.workspace_root)

    print(f"[RUN] Running script: {args.script}...")
    res = runner.run_python_script(script_path=args.script, args=args.args)
    if res.stdout:
        print(res.stdout.strip())
    if res.stderr:
        print(res.stderr.strip(), file=sys.stderr)
    return res.exit_code


def cmd_start(args: argparse.Namespace) -> int:
    cfg = load_config()
    daemon = OrchestraDaemon(cfg)
    print(f"[ORCHESTRA] Agent Orchestra Conductor started (watching {cfg.watch_paths})...")
    daemon.start(poll_interval=args.interval)
    return 0


def cmd_init(args: argparse.Namespace) -> int:
    cfg = OrchestraConfig()
    target = Path("orchestra.json")
    if target.is_file() and not args.force:
        print("orchestra.json already exists. Use --force to overwrite.")
        return 1
    cfg.save(target)
    print("[OK] Created orchestra.json with starter defaults.")
    return 0


def main(argv: Optional[list[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        prog="orchestra",
        description="Autonomous Multi-Agent Orchestration & IPC Event Bus",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    # status
    p_status = subparsers.add_parser("status", help="Show current state and lock information")
    p_status.add_argument("--json", action="store_true", help="Output as JSON")
    p_status.set_defaults(func=cmd_status)

    # claim
    p_claim = subparsers.add_parser("claim", help="Claim writer lock for a task")
    p_claim.add_argument("task_id", help="Task ID (e.g. AUTH-001)")
    p_claim.add_argument("--agent", default="agent", help="Agent identifier (e.g. claude, codex)")
    p_claim.add_argument("--scope", default="", help="Scope description")
    p_claim.add_argument("--lease", type=int, default=1800, help="Lease time in seconds")
    p_claim.add_argument("--force", action="store_true", help="Force claim even if currently held")
    p_claim.set_defaults(func=cmd_claim)

    # release
    p_release = subparsers.add_parser("release", help="Release writer lock and request handoff")
    p_release.add_argument("--agent", default=None, help="Agent releasing the lock")
    p_release.add_argument("--status", default="complete", choices=["complete", "paused", "failed"])
    p_release.add_argument("--summary", default="", help="Handoff summary")
    p_release.add_argument("--force", action="store_true", help="Force release")
    p_release.set_defaults(func=cmd_release)

    # emit
    p_emit = subparsers.add_parser("emit", help="Emit a custom event on the bus")
    p_emit.add_argument("event_type", help="Event type (e.g. CUSTOM_EVENT, REVIEW_VERDICT)")
    p_emit.add_argument("payload", nargs="?", default="{}", help="JSON payload string")
    p_emit.add_argument("--agent", default="cli", help="Source agent")
    p_emit.set_defaults(func=cmd_emit)

    # listen
    p_listen = subparsers.add_parser("listen", help="Stream incoming events live")
    p_listen.add_argument("--all", action="store_true", help="Stream all historical events first")
    p_listen.set_defaults(func=cmd_listen)

    # run
    p_run = subparsers.add_parser("run", help="Run a Python script through the orchestra runner")
    p_run.add_argument("script", help="Path to Python script")
    p_run.add_argument("args", nargs=argparse.REMAINDER, help="Arguments to pass to script")
    p_run.set_defaults(func=cmd_run)

    # start
    p_start = subparsers.add_parser("start", help="Start the autonomous orchestrator daemon")
    p_start.add_argument("--interval", type=float, default=2.0, help="Poll interval in seconds")
    p_start.set_defaults(func=cmd_start)

    # init
    p_init = subparsers.add_parser("init", help="Create a starter orchestra.json configuration")
    p_init.add_argument("--force", action="store_true", help="Overwrite existing configuration")
    p_init.set_defaults(func=cmd_init)

    parsed = parser.parse_args(argv)
    return parsed.func(parsed)


if hasattr(sys.stdout, "reconfigure"):
    try:
        sys.stdout.reconfigure(encoding="utf-8")
        sys.stderr.reconfigure(encoding="utf-8")
    except Exception:
        pass
    sys.exit(main())

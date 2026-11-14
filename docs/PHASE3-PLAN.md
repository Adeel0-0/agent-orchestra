# Phase 3 implementation plan

Owner authorized execution on 2026-11-15. Design: docs/PLAN.md sections 5–7.
Execute in the existing worktree without committing or pushing. Keep Phase 2 live
acceptance pending. Checkpoint/ownership ledger remains CHECKPOINT.md.

1. Store (`orchestra/app/store.py`, `tests/test_app_store.py`): SQLite projects,
   sessions, settings, event journal and player-state snapshots. Each event has a
   per-session sequence number. Commit events/state atomically; JSONL transcripts
   are repairable mirrors of the journal. On restart mark unfinished turns
   interrupted; never automatically rerun a command. Test replay offsets,
   restart, partial-mirror repair and strict settings/path validation.
2. Session manager (`orchestra/app/sessions.py`, `tests/test_app_sessions.py`):
   own one background turn per session independently of sockets; publish stored
   events through bounded subscriber queues; reconnect replays from a sequence
   offset. Persist CLI backend IDs and OpenRouter conversation snapshots (history,
   context and totals only, no clients/credentials). Same-folder concurrent turns
   are blocked until Phase 5 adds isolation. Test disconnect/reconnect, approval,
   cancellation, shutdown and restart using fake players and temporary stores.
3. Server (`orchestra/app/server.py`, `security.py`, `launcher.py`, CLI serve
   command, `tests/test_app_server.py`): all routes in docs/PLAN.md Phase 3,
   optional starlette/uvicorn app extra. Bind a preallocated socket on
   127.0.0.1:0; generate a launch-only token. Exact Host/Origin validation, reject
   forwarded authority, require Bearer token for HTTP and token-bearing WebSocket
   subprotocol. No query tokens, permissive CORS or unauthenticated route. Limit
   request/queue sizes; return clean validation/conflict errors. Read-only git
   diff against captured base; never mutate git. Test with ASGI/local sockets,
   no real agents or paid calls.
4. Integration: fake-player acceptance over real HTTP/WebSocket with UI disconnect
   while a turn runs, reconnect/replay and server restart. Run complete tests,
   compilation and diff checks; document launch/auth/recovery. Review changes and
   fix material findings. Update checkpoint and release writer ownership.

Decisions: SQLite event journal is authoritative so crashes cannot split events
from their player-state checkpoints; JSONL remains a readable/exportable mirror.
Slow subscribers disconnect and can replay; they cannot stall an agent. Restart
resumes conversation when a new prompt arrives, not an interrupted execution.
Real subscription and OpenRouter live acceptance remain outside this work.

Execution finished 2026-11-15. All four milestones completed. Final full supported
runtime suite: 127 tests, 126 passed, one existing symlink-privilege skip. Local
socket acceptance and wheel install/bootstrap smoke passed. Review findings and
their regression fixes are recorded in CHECKPOINT.md; no commits/pushes were made.

Published subsequently by owner request: implementation commit 40b06a9, PR #3
(base main, unmerged), annotated phase-3-offline milestone tag.

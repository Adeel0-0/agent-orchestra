# Local server and durable sessions (Phase 3)

Phase 3 supplies the backend used by the Phase 4 [window](WINDOW.md). It serves
HTTP and WebSocket APIs, runs one process on your machine, and stores local
conversation history, and uses the existing players and writer-lock engine.

## Start

Start the API only (no workspace UI):

```powershell
orchestra serve
```

`serve` reserves a random port on **127.0.0.1** and prints its URL and a new launch
token to your terminal. Keep the token private. It authorizes project access and
agent execution; it is never saved to disk or included in request URLs. Access logs
are disabled. Ctrl+C shuts down live turns and the server. `--data-dir <folder>`
selects a separate private data directory, useful for testing.

The app extra does not include OpenRouter credentials. Install `[app,openrouter]`
and use the hidden `openrouter-key` prompt separately when you choose to configure
that backend. Phase 2 live provider acceptance remains pending; no key setup or paid
model call was performed for this phase.

## HTTP

With API-only `serve`, every route requires `Authorization: Bearer <launch-token>`.
The UI launcher additionally serves an inert public `/` and `/assets/*`, plus
`POST /api/bootstrap` for its exact-Origin, one-use nonce exchange. Project/session
APIs stay authenticated in both modes. See [the bootstrap contract](PHASE4-BOOTSTRAP.md).
Use exactly the printed
`http://127.0.0.1:<port>` authority. `localhost`, other hosts/ports, foreign or `null`
Origins, duplicate authorization/authority headers and forwarded-authority headers
are rejected. An omitted Origin is allowed for native clients with the token.
Browser requests must use the exact local HTTP Origin. There is no permissive CORS.

JSON requests require `Content-Type: application/json` and are limited to 1 MiB.
Explicit validation, missing resource and conflicting turn/recovery errors return JSON with
400, 404 and 409 respectively; oversized bodies return 413. Successful message
submission returns 202 immediately; the turn belongs to the session manager.
Unexpected implementation exceptions remain server errors instead of being
misclassified as client mistakes.

| Method | Path | Body/result |
|---|---|---|
| GET | `/api/players` | Local player health and tools-capable OpenRouter catalog; no paid request |
| GET | `/api/capabilities` | Whether an authenticated native folder picker is available |
| POST | `/api/folders/pick` | Empty object; native folder path or null, or an error in browser-only mode |
| GET/POST | `/api/projects` | List projects / add `{"path":"D:/my-project"}` |
| GET/POST | `/api/projects/{pid}/sessions` | List / create with `player`, optional `model`, `title`, `permission` |
| POST | `/api/sessions/{sid}/messages` | `{"prompt":"Explain the parser"}` |
| POST | `/api/sessions/{sid}/cancel` | Cancel current turn; optional recovery confirmation described below |
| POST | `/api/sessions/{sid}/permissions/{rid}` | `{"allow":true}` or `false` |
| GET | `/api/sessions/{sid}/diff` | Saved Git base, tracked diff, untracked filenames and truncation flag |
| GET/PUT | `/api/settings` | `default_player`, `permission`; response also includes `openrouter_key_present` |

Settings never accept or return API keys. Health/key-presence checks do not establish
credit balance or access to a particular paid model. There is no default OpenRouter
model; select one explicitly when creating its session.

Git inspection disables external diffs, text conversion and repository filesystem
monitor commands. It never stages, commits, pushes or invokes a verification/ship
pipeline. Tracked diff output is capped at 1 MiB; untracked files are listed, not
loaded into the diff. A project without a Git base returns an empty diff with a null
base. Sessions use the selected folder; isolated worktrees arrive in Phase 5.

## Event stream and approvals

Connect to `ws://127.0.0.1:<port>/api/sessions/{sid}/events?after=<seq>` with browser
subprotocols `['orchestra', 'orchestra.token.' + launchToken]`. The server selects
`orchestra`. Native WebSocket clients may alternatively send the Bearer header.
The token is a subprotocol/header credential, never a query parameter.

Events keep the player event fields and add monotonic per-session `seq` and Unix
`timestamp`. `after=0` replays the whole session, then follows live events; an offset
beyond the last stored event is rejected. Save the last received `seq` to reconnect.
`message.user` and `permission.answered` are application transcript events alongside
the normalized player events.

A socket disconnect does not cancel a turn. Multiple subscribers receive the same
stored events. A slow subscriber is disconnected with code 1013 and can replay;
it cannot stall the agent. Approvals are scoped to the session and pending request
ID. A request remains pending when the UI disappears; reconnect and answer it, or
cancel the session. Restart invalidates pending approval IDs.

## Storage and recovery

Default files are under `~/.orchestra/`:

- `orchestra.db`: projects, sessions, settings, authoritative event journal and
  atomic player-state snapshots.
- `sessions/<id>.jsonl`: readable transcript mirrors, repaired from the journal on
startup if missing or the byte length disagrees with the durable journal. Complete
same-size mirrors are not rewritten; this lightweight check does not detect
same-length manual corruption.
- `server.lock`: OS-held single-server lock for that data directory. Closing/crashing
  the process releases it; the file itself can remain.

SQLite commits events and their player snapshots together, and commits terminal
session status with the terminal event. JSONL is a recoverable projection so a
crash between the two writes cannot lose the durable event. Transcripts contain
prompts, outputs and source context in ordinary local storage; keep the data folder
private. API client/key objects and launch tokens are excluded from snapshots.
Unchanged history is not serialized again for every text delta. Completed idle
players are evicted from memory and reconstructed from durable state on demand.

CLI players resume from the saved backend session ID. OpenRouter resumes from saved
conversation/history and accumulated usage. This durable resume is managed by the
server; standalone `orchestra chat -a openrouter --resume` remains unsupported.

After a clean server restart, completed conversations accept follow-up prompts with
their saved context. A turn running during an abrupt restart becomes `interrupted`.
It is never automatically rerun. Unanswered OpenRouter tool calls receive an
interruption result explaining that the tool may already have run.

**An abrupt server crash does not prove every CLI descendant stopped.** Inspect and
stop leftover agent/command processes before continuing an interrupted session.
After doing so, explicitly send `{"confirm_stopped":true}` to its cancel endpoint.
This acknowledges recovery, clears that session's saved blocked state and releases
only its own stale app writer lease. Then submit a new prompt to continue the
conversation. The confirmation does not itself stop an orphaned process.

Failed cleanup similarly records `cleanup_failed` and blocks overlapping project
turns. Its writer lease is retained and extended where possible until recovery is
confirmed. Lease-renewal failures stop the turn even if writing the diagnostic also
fails. Native writer locks are cooperative; unrelated processes are not sandboxed.

The manager allows one turn per session and prevents concurrent turns in the same
or overlapping folders. It claims/renews/releases the existing engine writer lock;
no handoff event is emitted, so ending a chat never triggers automatic publishing.
Per-launch writer identities prevent silent reacquisition of another launch's lease.

## Verification

```powershell
go test ./internal/app/...
```

Tests use temporary stores,
repositories and fake players. The real local HTTP/WebSocket acceptance test
disconnects during a running turn, reconnects/replays, restarts the server and
submits a follow-up with the persisted backend ID. API-player snapshot tests cover
history/usage restore and interrupted tool repair. No real subscription or paid API
call is required. Manual console Ctrl+C, abrupt real-CLI crash cleanup, actual
provider resume and Linux/macOS acceptance remain separate checks.

History (Python version): verified on 2026-11-15 in an isolated Windows Python 3.12 environment with
Starlette 0.52.1, Uvicorn 0.54.0, websockets 16.1.1 and httpx 0.28.1: the full suite
ran 156 tests in 59.413 seconds (155 passed, one symlink-privilege skip), including
the Phase 4 UI/bootstrap/runtime checks. Browser acceptance uses a fake provider
over real HTTP/WebSocket and asserts clean server teardown. A wheel with bundled
UI assets was built and installed into an isolated target for static-shell,
bootstrap/authentication and runtime shutdown checks. No global installation or
OpenRouter credential setup is required. Native smoke and other acceptance limits
are recorded in CHECKPOINT.md; live provider acceptance remains pending.

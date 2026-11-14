# Agent Orchestra → a simple, self-owned coding-agent app

Status: Phase 0 landed on `main` through PR #1 (OS-level lock guard, `policy.should_ship`, strict config, process-tree timeouts); this branch adds `__main__.py`, the import-time `sys.exit` fix and the CI token fix. Phase 1 implemented and tested for Codex and Claude; the Antigravity player follows the official docs but has not run, because `agy` is not installed. Phases 2 to 7 not started. Updated 2026-11-14.

## 1. The goal in one paragraph

Type `orchestra` in a terminal. A window opens. You pick a project folder, pick an agent
(ChatGPT Plus, Google AI Pro, or any OpenRouter model), and chat with it while it reads,
edits and runs code in that project, the way Claude Code Desktop works. Everything runs
on your machine, costs nothing beyond the subscriptions you already pay for, and stays
small enough that you can read all of it.

## 2. What exists today, and the gap

Today Orchestra is a **coordinator**, not an agent. It is 1,600 lines of dependency-free
Python: a JSONL event bus, a single-writer lock, a filesystem watcher, a script runner and
a git sync daemon. It never talks to a model. Agents are expected to call
`orchestra claim` / `orchestra release` themselves.

| You want | Today | Gap |
|---|---|---|
| Open a terminal or a UI | CLI only, and `python -m orchestra` is broken | Launcher, local server, UI |
| Open a project like Claude Code Desktop | Config is one `orchestra.json` per repo | Project registry, sessions, transcripts |
| Use ChatGPT Plus | Nothing | Codex CLI adapter |
| Use Google AI Pro | Nothing | Antigravity CLI adapter |
| Use OpenRouter models | Nothing | Own agent loop with tools |
| Stay simple | Yes | Keep it that way: one process, one protocol |

The existing pieces are worth keeping. The event bus becomes the transcript stream, the
lock becomes per-worktree ownership, and the verify/git pipeline becomes the "check the
agent's work" step that most chat apps lack.

## 3. The one fact that shapes the design

**A subscription is not an API key.** ChatGPT Plus and Google AI Pro do not give you an
endpoint you can call. The supported way to spend those subscriptions from your own
software is to drive each vendor's official CLI, which holds the login.

Checked on this machine and online on 2026-11-14:

| Account | Supported route | State here |
|---|---|---|
| ChatGPT Plus | `codex exec --json` (Codex CLI) | Installed, v0.166.0, reports "Logged in using ChatGPT" |
| Google AI Pro | `agy -p --output-format stream-json` (Antigravity CLI) | Not installed |
| OpenRouter | Plain HTTPS API with a key | Needs a key |
| Claude (bonus) | `claude -p --output-format stream-json` | Installed, v2.1.319 |

Two warnings:

- **Gemini CLI is no longer the route for Google AI Pro.** Its repo says personal AI Pro
  and Ultra access moved to Antigravity CLI on 2026-06-18, and bug reports show AI Pro
  sign-ins being pushed to an API-key flow. I read this in search results, not on
  Google's own migration page, so Phase 1 starts by confirming it with a real login.
- **Do not copy OAuth tokens out of the CLIs to call private endpoints.** That is what
  some "free" tools do. It breaks the terms of service, risks the account, and breaks
  whenever the vendor changes something. This plan never does it.

So there are two kinds of player:

- **CLI players** (Codex, Antigravity, Claude): the CLI is the agent. Orchestra starts
  it, feeds it a prompt, and translates its JSON events.
- **API players** (OpenRouter): there is no agent, only a model. Orchestra supplies the
  loop and the tools.

## 4. Decisions

| # | Decision | Choice | Why |
|---|---|---|---|
| D1 | Terminal or UI | Both from one command. `orchestra` opens the window, `orchestra chat` stays in the terminal | Same server behind both, so no duplicated logic |
| D2 | Window technology | Local web UI served by the Python process, shown in a native window with `pywebview`; `--browser` opens it in Chrome instead | No Rust on this machine, so Tauri is out. Electron is 150 MB of the complexity you are avoiding |
| D3 | Backend language | Stay in Python 3.12 | The engine is already Python and tested |
| D4 | Server | `starlette` + `uvicorn`, installed as an optional extra `orchestra[app]` | Needs WebSockets. The core package stays dependency-free |
| D5 | Frontend | React + Vite + TypeScript, built once and shipped inside the package | Streaming chat, diffs and permission prompts get messy in plain JS. Node 24 is installed |
| D6 | Talking to CLIs | One thin adapter per CLI over its documented headless JSON mode | See 4.1 |
| D7 | Storage | Stdlib `sqlite3` index plus one JSONL transcript per session, under `~/.orchestra/` | No database server. Transcripts stay greppable |
| D8 | Secrets | OpenRouter key in Windows Credential Manager via `keyring` | Never in `orchestra.json`, never in git |
| D9 | Isolation | Each session can run in its own git worktree | Lets two agents work at once without the single-writer lock blocking them |

### 4.1 Why not ACP everywhere

The Agent Client Protocol is a standard for exactly this job and has a Python SDK
(`agent-client-protocol` 1.0.0). It is tempting. But only Gemini CLI speaks it natively.
Codex needs the `codex-acp` adapter, and Antigravity has no native ACP mode, only
community bridges that wrap the same stream-json flags this plan uses directly. Going
through ACP today means three extra programs between you and the model.

The plan: define Orchestra's own small event schema (section 6), write direct adapters
first, and add a generic ACP adapter in Phase 7. That later adapter unlocks every
ACP agent at once without changing anything above it.

## 5. Architecture

```
  orchestra (launcher)
      │
      ├── pywebview window ──┐
      └── terminal chat ─────┤  HTTP + WebSocket, 127.0.0.1 only
                             ▼
                    ┌─────────────────┐
                    │  Server         │  projects, sessions, permissions
                    │  SessionManager │
                    └───────┬─────────┘
                            │ Player interface (section 6)
        ┌───────────┬───────┴────┬──────────────┐
        ▼           ▼            ▼              ▼
   CodexPlayer  AgyPlayer   ClaudePlayer   OpenRouterPlayer
   codex exec   agy -p      claude -p      own loop + tools
   (ChatGPT)    (AI Pro)                   (HTTPS API)
        │           │            │              │
        └───────────┴────────────┴──────────────┘
                            ▼
              project folder or git worktree
                            ▼
        existing engine: EventBus, LockManager, verify, git sync
```

### 5.1 File layout

```
orchestra/
  __main__.py            NEW   makes `python -m orchestra` work
  cli.py                 EDIT  add: ui, chat, agents, doctor
  bus.py, models.py, ... EDIT  fixes from Phase 0
  players/               NEW
    base.py                    Player interface, normalized events
    process.py                 async subprocess with line streaming and tree kill
    codex.py                   codex exec --json
    antigravity.py             agy stream-json
    claude.py                  claude stream-json
    openrouter.py              agent loop over chat completions
    registry.py                detect installed CLIs and login state
  tools/                 NEW   used only by API players
    fs.py                      read_file, list_dir, glob, grep, edit_file, write_file
    shell.py                   run_command
    policy.py                  permission decisions
  app/                   NEW
    server.py                  routes and WebSocket
    sessions.py                SessionManager
    store.py                   sqlite index and JSONL transcripts
    worktrees.py               create, list, remove
    launcher.py                start server, open window
    web/dist/                  built frontend
web/                     NEW   frontend source (Vite)
```

## 6. The contract everything hangs on

Every player, whatever is behind it, emits the same events. The UI, the transcript
store and the orchestration modes only ever see these.

```python
class Player(Protocol):
    id: str                                   # "codex", "antigravity", "openrouter"
    async def start(self, cwd: Path, model: str | None, resume: str | None) -> None: ...
    def send(self, prompt: str, images: list[Path] = ()) -> AsyncIterator[Event]: ...
    async def answer_permission(self, request_id: str, allow: bool) -> None: ...
    async def cancel(self) -> None: ...
```

| Event | Payload | Notes |
|---|---|---|
| `session.started` | `backend_session_id`, `model` | The id lets a session resume later |
| `text.delta` | `text` | Streamed assistant text |
| `reasoning.delta` | `text` | Shown collapsed |
| `tool.started` | `id`, `name`, `input` | A card in the UI |
| `tool.output` | `id`, `chunk` | Live command output |
| `tool.finished` | `id`, `ok`, `summary` | |
| `permission.requested` | `request_id`, `tool`, `input`, `reason` | Turn pauses until answered |
| `file.changed` | `path`, `kind` | Drives the diff pane |
| `usage` | `input_tokens`, `output_tokens`, `cost_usd` | Cost is known only for OpenRouter |
| `turn.completed` | `stop_reason` | |
| `error` | `message`, `retryable` | Includes "not logged in" and "quota reached" |

These extend the existing `EventType` enum in `orchestra/models.py`, so a chat turn and
an `AGENT_CLAIM` travel on the same bus.

## 7. Phases

Each phase ends in something you can run. Do not start a phase before the previous
one's check passes.

### Phase 0: make the engine safe to build on (1 to 2 days)

Problems found while reading the code:

| Where | Problem | Fix |
|---|---|---|
| `orchestra/` | No `__main__.py`. `python -m orchestra`, the README's first command, fails | Add it |
| `cli.py:237` | `sys.exit(main())` sits inside the `if hasattr(...)` block, so importing the module runs the CLI | Move under `if __name__ == "__main__"` |
| `bus.py:164` | Lock is read, then written. Two agents claiming together both succeed | Create with `os.open(..., O_CREAT \| O_EXCL)`; only replace an expired lock |
| `daemon.py:139` | Any dirty tree with no lock triggers verify, then `git add .`, commit, rebase and push. Your own half-typed edit gets pushed to `main` | Run the pipeline only after a `HANDOFF_REQUEST`, once per handoff |
| `daemon.py:125` | A failed verify sets `ERROR`, the next tick clears it and reruns verify every 2 seconds forever | Hold `ERROR` until a new handoff or a manual reset |
| `daemon.py:204` | Rebase result is ignored and the push happens anyway | Stop and emit an error event when rebase fails |
| `git_sync.py:75` | `fast_forward_branch` checks out another branch in the live working tree | Use `git push origin main:branch` or `git fetch . main:branch` |
| `config.py:21` | `auto_push` defaults to true | Default false |
| `ci_tracker.py:47` | `git credential fill` runs with no input and can wait out the 300 s timeout. The daemon never uses `CiTracker` | Use `gh auth token`; wire in or remove |
| `bus.py:85` | Every read parses the whole `events.jsonl`, which grows forever | Tail from an offset; rotate by size |
| `runner.py:71` | `subprocess.run` returns output only at the end | Add the async streaming runner that players need |

Check: existing 10 tests pass, plus new tests for the lock race, the handoff trigger and
`python -m orchestra status`.

### Phase 1: prove the subscriptions work, in the terminal (3 to 4 days)

No UI yet. This is the phase that can fail for reasons outside your control, so it goes first.

1. `players/process.py`: start a child with `asyncio`, yield stdout lines as they arrive,
   capture stderr, and kill the whole process tree on cancel. On Windows `codex` is an
   npm `.cmd` shim, so resolve it with `shutil.which` and kill with `taskkill /T /F`.
2. `players/codex.py`: run `codex exec --json -C <project> -s workspace-write -m <model> -`,
   prompt on stdin. Map its JSON lines to section 6 events. Save the session id from the
   first event so follow-up turns use `codex exec resume <id>`.
3. `players/antigravity.py`: install `agy`, log in once by hand with the AI Pro account,
   then run `agy -p --output-format stream-json --input-format stream-json`. Before
   writing the mapper, record real output to a fixture file.
4. `players/claude.py`: same pattern, optional.
5. `players/registry.py` and `orchestra doctor`: for each CLI report installed, version,
   logged in, and the exact command to fix it.
6. `orchestra chat --agent codex "explain this repo"`: prints the normalized stream.

Check: one prompt that edits a file succeeds through Codex and through Antigravity, a
second prompt in the same session remembers the first, and Ctrl+C leaves no orphan process.

Known limit: `codex exec` cannot pause to ask permission. The sandbox flag is the
guard rail: `read-only` for "ask" mode, `workspace-write` for normal use. Real
per-command approvals for Codex arrive in Phase 7 through `codex app-server`.

### Phase 2: your own agent for OpenRouter (4 to 5 days)

This is the only place Orchestra is the agent, and the part most worth writing yourself.

1. Client: `POST https://openrouter.ai/api/v1/chat/completions` with `stream: true`,
   parsed as server-sent events using stdlib `urllib`. Model list from `/api/v1/models`,
   filtered to models that support tools.
2. Loop: send messages and tool schemas, stream text, collect tool calls, run them,
   append results, repeat until the model stops calling tools. Cap at 50 steps.
3. Tools: `read_file`, `list_dir`, `glob`, `grep`, `edit_file` (exact string replace,
   fails if the match is not unique), `write_file`, `run_command` (timeout, output cap).
   Every path is resolved and rejected if it leaves the project root.
4. Policy: three modes. Ask for every write and command, accept edits but ask for
   commands, or allow everything inside the project.
5. Context: a system prompt with the OS, the project root and the contents of
   `AGENTS.md` or `CLAUDE.md` when present. When the transcript nears the model's
   limit, summarize older turns.
6. Cost: sum the `usage` field per session.

Check: with a cheap tool-capable model, "add a test for X and run it" completes, a
`../` path is refused, and a denied command is reported back to the model without crashing.

### Phase 3: server and sessions (3 to 4 days)

- Store: `~/.orchestra/orchestra.db` with `projects(id, path, name, last_opened)` and
  `sessions(id, project_id, player, model, title, worktree, backend_session_id, status, created, updated)`.
  Transcript at `~/.orchestra/sessions/<id>.jsonl`.
- SessionManager: owns live players, one turn at a time per session, fans events out to
  the store and to every connected socket.
- API:

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/players` | Installed, logged in, models |
| GET, POST | `/api/projects` | List, add by folder path |
| GET, POST | `/api/projects/{id}/sessions` | List, create |
| WS | `/api/sessions/{id}/events` | Replay from an offset, then live |
| POST | `/api/sessions/{id}/messages` | Send a prompt |
| POST | `/api/sessions/{id}/cancel` | Stop the turn |
| POST | `/api/sessions/{id}/permissions/{rid}` | Allow or deny |
| GET | `/api/sessions/{id}/diff` | Changes against the session's base commit |
| GET, PUT | `/api/settings` | Default player, permission mode, key present or not |

- Safety: a local server that can run shell commands is a target for any web page you
  visit. Bind to `127.0.0.1` on a random port, require a random token generated at
  launch, and reject requests whose `Origin` or `Host` is not the local address.

Check: start a turn, kill the UI, reconnect, and the transcript replays and continues.
Restart the server and the session resumes with its history.

### Phase 4: the window (6 to 8 days)

Three columns, the same shape as Claude Code Desktop:

- Left: projects, and sessions under each, with a status dot.
- Centre: transcript with streaming text, tool cards that expand to show input and
  output, inline permission prompts, and a composer with a player and model picker.
- Right: changed files and a diff view.

Also: an "Open folder" dialog through pywebview's native picker, a first-run screen
built from `orchestra doctor`, a settings page, light and dark themes, and keyboard
shortcuts for new session, stop and switch project.

Check: from a cold start, open a folder, send a prompt, approve an edit, read the diff,
and switch model, without touching the terminal.

### Phase 5: worktrees and review (3 to 4 days)

- New session option: "isolated". Creates `.orchestra/worktrees/<slug>` on a new branch.
- The single-writer lock becomes one lock per worktree, so parallel sessions are legal
  and two sessions in the same folder are still blocked.
- Diff pane gains: commit, discard, merge back, open a PR through `gh`.
- The existing `verify_command` runs on demand or after each turn, and its result shows
  as a pass or fail badge on the session.

Check: two sessions with different players edit the same file in parallel without
touching each other's work.

### Phase 6: the orchestra part (4 to 5 days)

This is what the paid tools charge for, and what the name promises.

- **Relay**: player A implements, player B reviews the diff, A fixes, and the loop ends
  when B approves or after N rounds. Uses the existing `REVIEW_REQUEST` and
  `REVIEW_VERDICT` events.
- **Race**: the same prompt goes to several players in separate worktrees. You compare
  diffs and verify results side by side and keep one.
- **Budget routing**: cheap OpenRouter model for exploration, subscription player for
  the edit. Falls over to another player when one reports a quota error.

Check: a relay between Codex and an OpenRouter reviewer finishes a small task
unattended and leaves a verified diff.

### Phase 7: polish and reach (ongoing)

- Per-command approvals for Codex through `codex app-server` (its JSON schema can be
  generated with `codex app-server generate-json-schema`).
- Generic ACP player using the `agent-client-protocol` SDK.
- MCP servers for the OpenRouter player.
- Image paste, slash commands, session search, desktop notifications.
- Packaging: `uv tool install`, a Start menu shortcut, and a single-file build.

## 8. Risks

| Risk | Likelihood | What to do |
|---|---|---|
| Vendor changes or restricts headless use of a subscription CLI | Medium. Google did it to Gemini CLI this year | Adapters are 150 lines each and isolated. OpenRouter always works as a fallback |
| CLI JSON formats change between versions | High | Record fixtures per CLI version, test mappers against them, show the detected version in `doctor` |
| Subscription quotas are lower than expected for agent use | Medium | Show quota errors clearly. Budget routing in Phase 6 |
| Agent runs a destructive command | Medium | Sandbox flags for CLIs, policy for the OpenRouter player, worktrees by default from Phase 5 |
| Scope creep toward a full IDE | High | No editor, no terminal emulator, no plugin system before Phase 7 |
| Windows process and encoding quirks | High | Handled once in `players/process.py`, tested on this machine first |

## 9. Testing

- Unit: mappers against recorded fixtures, tool path confinement, policy decisions, lock race.
- Fake player: emits a scripted event sequence, so the server and UI are testable with
  no network and no subscription.
- End to end: one smoke script per real player, run by hand before a release since it
  spends quota.

## 10. Rough size

About 28 to 36 working days for Phases 0 to 6. After Phase 1 (one week) you already
have a terminal tool that drives ChatGPT Plus and Google AI Pro. After Phase 4 (about
four weeks) you have the app you described.

## 11. First commands when starting

```
python -m unittest discover tests
codex exec --json -s read-only "list the files in this repo" > tests/fixtures/codex_0.166.jsonl
```

The second command captures real Codex output, which is the input for the first mapper.

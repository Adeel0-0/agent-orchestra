# 🎻 Agent Orchestra

> **Autonomous Multi-Agent Coordination, File-Based IPC Event Bus, and Reactive Execution Engine.**
> Built for pair-programming and autonomous workflows involving **Claude Code**, **OpenAI Codex**, **Google Antigravity**, **Cursor**, and automated pipelines.

---

## 🌟 Why Agent Orchestra?

Modern AI software development uses multiple specialized autonomous agents working concurrently:
- One agent implements backend code (e.g. Codex)
- Another agent reviews or refactors (e.g. Claude Code)
- A supervisory agent handles git synchronization, test verification, and CI tracking (e.g. Git Engine / Antigravity)

**The Problem**:
Without a conductor, multi-agent setups suffer from:
1. **Race conditions**: Two agents edit files at the same time, producing merge conflicts or overwritten code.
2. **Siloed communication**: Agents cannot easily message each other across separate terminal sessions without manual human copy-pasting.
3. **Manual handoffs**: Developers have to manually run verification test suites, stage commits, and push branches after an agent finishes.

**The Solution**:
**Agent Orchestra** acts as the autonomous conductor. It provides:
- **Instant Reactive Detection**: Detects the moment an agent touches code or claims a task via an event-driven filesystem watcher.
- **File-Based IPC Event Bus**: Distributed pub/sub message broker (`events.jsonl`) that works across all CLI agents with zero external dependencies.
- **Single-Writer Lock Manager**: Prevents multiple agents from stepping on each other; enforces strict atomic ownership leases.
- **Automated Script Execution**: Automatically runs Python verification scripts, unit test suites, or contract probes upon agent handoffs.
- **Autonomous Git Sync**: Auto-commits with structured conventional messages, rebases on remote, and fast-forwards secondary branches.
- **CI Monitoring**: Connects to remote CI (GitHub Actions) using native Git credential helpers without leaking tokens.

---

## 📦 Zero-Dependency, Standalone Architecture

Agent Orchestra is engineered to be **100% self-contained**:
- Written in modern Python 3.10+ using only standard libraries (`json`, `subprocess`, `threading`, `pathlib`, `urllib`).
- Zero mandatory external packages.
- Works across **Windows (PowerShell/CMD)**, **Linux (Bash/Zsh)**, and **macOS**.
- Designed to be **cut and pasted into any repository** or published as an independent package on GitHub.

```
agent-orchestra/
├── pyproject.toml              # Modern Python packaging & CLI entry points
├── README.md                   # Full documentation & usage guide
├── .gitignore                  # Ignores runtime events, locks, and pycache
├── orchestra.default.json      # Starter configuration for any codebase
├── orchestra.sefgh.json        # Production configuration tailored for SEFGH
├── orchestra/
│   ├── __init__.py             # Public API exports
│   ├── cli.py                  # Full CLI interface (`orchestra claim`, `release`, etc.)
│   ├── daemon.py               # Central conductor event loop & state machine
│   ├── bus.py                  # File-based IPC event bus & distributed lock manager
│   ├── models.py               # JSON-serializable dataclasses & event schemas
│   ├── watcher.py              # Debounced filesystem watcher with mtime tracking
│   ├── runner.py               # Python & shell script execution engine
│   ├── git_sync.py             # Git staging, commit, rebase, and multi-branch sync
│   └── ci_tracker.py           # GitHub Actions workflow run tracker
├── scripts/
│   ├── example_verifier.py     # Sample Python verification script emitting JSON
│   ├── example_notifier.py     # Sample webhook/notification script
│   └── sefgh_bridge.py         # Bridge for docs/coordination.md integration
└── tests/
    ├── test_bus.py             # IPC bus & lock manager tests
    ├── test_config.py          # Config loader tests
    ├── test_models.py          # Event serialization tests
    └── test_runner.py          # Script runner & timeout tests
```

---

## 🚀 Quickstart

### Open the coding-agent app

```powershell
python -m pip install -e ".[desktop]"
# Source checkout only: build the bundled frontend once.
cd web
npm ci
npm run build
cd ..
python -m orchestra
```

No arguments opens the native window. `python -m orchestra ui --browser` opens the
same app in your browser using `.[app]` dependencies; enter a local folder path
there. The native window uses the OS folder picker. Packaged wheels include the
built frontend and do not need Node. Agent CLIs must already be installed and
signed in; the first-run screen reports their health. OpenRouter setup remains a
separate hidden keyring prompt and an explicitly selected model.

The window provides projects/sessions, streaming chat and approvals, a composer
with agent/model selection, changed-file diffs, settings and light/dark themes.
Changing agent/model starts a fresh session. Closing the native window stops its
local server and agents; browser mode stops with terminal Ctrl+C. Reloading a page
requires relaunching the app for a fresh one-use bootstrap link. See
[the Phase 4 window guide](docs/WINDOW.md) for verification and limitations.

The original coordination commands remain available below. Phase 4 is verified
offline with fake providers; real provider acceptance is still pending.

### 1. Installation

Run directly with Python, or install in editable mode:

```bash
# Direct execution:
python -m orchestra --help

# Or install locally:
pip install -e .
```

### 2. Initialize in Any Project

```bash
orchestra init
```
This generates an `orchestra.json` configuration in your root directory.

The config is checked strictly. An unknown key (such as a typo like `auto_psuh`), a malformed file, or a section that isn't an object stops every command with an error naming the problem. Keys you leave out keep their defaults. `workspace_root` is resolved relative to the folder holding the config file.

---

## 💬 Chat with an Agent from the Terminal

Orchestra drives the official agent CLIs with your existing logins, so ChatGPT Plus or Google AI Pro subscriptions are used directly, with no API keys.

```bash
orchestra doctor                      # which agents are installed and logged in
orchestra chat "explain this repo"    # one turn with Codex (ChatGPT) in the current folder
orchestra chat -a claude -C ../app    # a conversation with Claude Code in another folder
orchestra chat -r <session-id> "and now add tests"   # continue a previous session
```

| Agent (`-a`) | Account | Needs |
|---|---|---|
| `codex` (default) | ChatGPT Plus / Pro | `npm install -g @openai/codex`, then `codex login` |
| `antigravity` | Google AI Pro / Ultra | Antigravity CLI (`agy`), signed in once interactively |
| `claude` | Claude Pro / Max | Claude Code, signed in |

`-p` sets what the agent may do: `read-only`, `edit` (default) or `full` (no limits). `--json` prints the normalized event stream instead of formatted text. Ctrl+C stops the current turn and the agent's processes.

What `edit` means for each agent, checked against the real CLIs on 2026-11-21:

| Agent | `edit` uses | Edits files | Runs commands |
|---|---|---|---|
| Codex | `-s workspace-write` | yes | yes, in Codex's sandbox |
| Antigravity | `--sandbox` | yes | yes, in Antigravity's sandbox |
| Claude Code | `--permission-mode auto` | yes | yes, **not sandboxed**; Claude Code's own safety checker blocks risky actions (its sandbox is not available on Windows) |
| OpenRouter | Orchestra's own policy | yes | asks first |

`read-only` maps to each CLI's plan or read-only mode; `full` turns off every check.

---

## 🛠️ How Agents Communicate

Agents interact with the Orchestra using simple CLI commands or the Python API:

### 1. Claiming a Task (Acquiring Writer Lease)
When Claude Code or Codex starts a task:
```bash
orchestra claim AUTH-001 --agent claude --scope "OAuth2 token rotation" --lease 1800
```
- Emits an `AGENT_CLAIM` event on the IPC bus.
- Creates `.orchestra/writer.lock`.
- Other agents trying to claim will be blocked until the lease expires or is released.
- Claims are atomic across processes: if several agents claim at once, exactly one wins.

### 2. Checking Current State
Any agent or human can check the real-time status:
```bash
orchestra status
```
```text
=== AGENT ORCHESTRA STATUS ===
State:        WRITER_ACTIVE
Is Locked:    True
  Task:       AUTH-001
  Agent:      claude
  Scope:      OAuth2 token rotation
Git Branch:   main (HEAD: 732d0dc)
Git Clean:    False (3 changed files)
Latest Event: AGENT_CLAIM by claude
```
Or as JSON for machine parsing:
```bash
orchestra status --json
```

### 3. Releasing & Requesting Handoff
When an agent finishes implementation:
```bash
orchestra release --agent claude --status complete --summary "Implemented JWT rotation and 12 unit tests"
```
- Emits a `HANDOFF_REQUEST` event carrying the status and the lock that was released.
- Removes `.orchestra/writer.lock`.
- The daemon passes the handoff to `should_ship()` in `orchestra/policy.py`. Only handoffs it approves are verified, committed, and pushed.
- `--status paused` and `--status failed` never ship. The daemon reports `PAUSED_CHECKPOINT` and leaves the work untouched.

### 4. Emitting Custom Events
Agents can broadcast custom telemetry, review verdicts, or notifications:
```bash
orchestra emit REVIEW_VERDICT '{"task_id": "AUTH-001", "accepted": true, "comments": "All tests pass"}' --agent codex
```

### 5. Listening Live to the Event Bus
Stream all agent activities in real-time:
```bash
orchestra listen
```
Output:
```text
[LISTEN] Listening to Agent Orchestra event bus (Ctrl+C to stop)...
[AGENT_CLAIM] (claude) -> {"task_id": "AUTH-001", "scope": "OAuth2 token rotation"}
[TASK_PROGRESS] (claude) -> {"changed_count": 3, "sample_files": ["src/auth.py"]}
[HANDOFF_REQUEST] (claude) -> {"task_id": "AUTH-001", "status": "complete"}
[VERIFICATION_STARTED] (orchestra) -> {"command": "python scripts/example_verifier.py"}
[VERIFICATION_RESULT] (orchestra) -> {"success": true, "duration_seconds": 1.2}
[GIT_COMMITTED] (orchestra) -> {"commit_message": "chore(orchestra): automated multi-agent handoff sync"}
[GIT_SYNCED] (orchestra) -> {"main_branch": "main", "synced_branches": ["go-backend"]}
```

---

## 🐍 Triggering Python Scripts

Agent Orchestra can execute custom Python scripts at every phase of the lifecycle:

```json
{
  "pipeline": {
    "pre_claim_command": "python scripts/pre_flight.py",
    "verify_command": "python scripts/example_verifier.py",
    "post_sync_command": "python scripts/example_notifier.py",
    "on_error_command": "python scripts/alert_failure.py"
  }
}
```

### Python Script Execution via CLI
You can also run any Python script directly through the Orchestra runner:
```bash
orchestra run scripts/example_verifier.py --verbose
```
The runner:
- Enforces execution timeouts, killing the command and everything it started (for example `npm` and the `node` processes under it)
- Captures stdout and stderr cleanly
- Automatically extracts JSON metrics printed by the script into structured execution reports

---

## 🎻 Running the Autonomous Conductor Daemon

To have the Orchestra run continuously in the background:
```bash
orchestra start --interval 2.0
```
The daemon will:
1. Watch watched paths for file modifications.
2. Read new `HANDOFF_REQUEST` events from `events.jsonl`, resuming from `.orchestra/daemon_cursor.json` after a restart. On its first start it skips existing history.
3. Run the verification pipeline once per approved handoff, holding the writer lock as `orchestra` so no agent can start editing mid-pipeline. A dirty tree with no handoff is only reported, never committed.
4. When verification passes, commit on `main_branch`. With `auto_push`, it then fetches, rebases onto the remote, pushes, and moves each `sync_branches` entry forward on the remote (`git push origin main:<branch>`, with no checkout). If any step fails, the sync stops there: a failed rebase is aborted, nothing more is pushed, a `GIT_SYNC_FAILED` event names the step, and the state goes to `ERROR`. If a different branch is checked out, nothing is committed.
5. Publish all lifecycle events to `events.jsonl`, including `CUSTOM_EVENT` with `"action": "handoff_skipped"` when a handoff is not shipped.
6. Hold `ERROR` after a failed verification or sync until the next writer claims the lock.

---

## 📂 How to Extract into a Standalone GitHub Repository

To use this in other projects (`Outcome Verifier`, `campus360-api`, etc.):

1. **Cut or Copy the folder**:
   ```bash
   cp -r d:/SEFGH/sefgh-nextjs-main/agent-orchestra d:/projects/agent-orchestra
   ```
2. **Push to GitHub**:
   ```bash
   cd d:/projects/agent-orchestra
   git init
   git add .
   git commit -m "feat: initial commit of Agent Orchestra engine"
   git remote add origin https://github.com/Adeel0-0/agent-orchestra.git
   git push -u origin main
   ```
3. **Drop it into any project**:
   - As a submodule: `git submodule add https://github.com/Adeel0-0/agent-orchestra.git tools/orchestra`
   - Or install via pip: `pip install git+https://github.com/Adeel0-0/agent-orchestra.git`
   - Run `orchestra init` in that new repo and configure `orchestra.json`.

---

## OpenRouter terminal agent

See [docs/OPENROUTER.md](docs/OPENROUTER.md) for keyring setup, tool-capable models,
permissions and verification. Start with `python -m pip install -e ".[openrouter]"`,
then `python -m orchestra openrouter-key`, `python -m orchestra models` and
`python -m orchestra chat -a openrouter -m <model-id>` in an interactive terminal.

## Local API and sessions

The Phase 3 [local API](docs/LOCAL-API.md) provides durable sessions and protected
HTTP/WebSocket access. Install `.[app]` and run `python -m orchestra serve`; it prints
a random loopback URL and private launch token. Use `orchestra ui` for the
[Phase 4 window](docs/WINDOW.md).

## 🧪 Running Unit Tests

```bash
cd agent-orchestra
python -m unittest discover tests
```
The suite covers models, event serialization, single-writer locking (including a multi-process claim race), the handoff-driven pipeline and git sync on throwaway repos with a bare remote, the shipping policy, script timeouts, strict configuration, entry points, recorded Codex/Claude event translation and cancellation. OpenRouter tests add confined file tools, approval/denial, shell timeout/output limits, SSE parsing and local HTTP streaming, context summaries and usage accounting. Tests do not use real API credentials or credits.

---

## 📜 License
MIT License. Created by Syed Mohammad Adeel.


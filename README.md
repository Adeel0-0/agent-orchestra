# 🎻 Agent Orchestra

> **Your own coding-agent app, plus a safety engine for several agents sharing one repository.**
> Use your ChatGPT Plus (Codex), Claude (Claude Code), Google AI Pro (Antigravity) or any OpenRouter model to read, edit and test code in a project folder, from the terminal or a browser workspace. One Go binary, no runtime to install.

---

## 🌟 What it does

**The app.** Open a project folder, pick an agent, and chat with it while it works in your code. Orchestra drives the official agent CLIs with the logins you already have, so subscriptions are used directly with no API keys (OpenRouter is the exception: it uses your OpenRouter key, kept in the OS keyring). Every conversation is saved, so you can close the workspace, reopen it and continue.

**The engine.** When several agents (or people) work in one checkout, Orchestra stops them overwriting each other and only commits finished, tested work:
- **Single-writer lock**: an agent claims the folder before editing; claims are atomic across processes, so if several agents claim at once exactly one wins.
- **Event bus**: an append-only `events.jsonl` that every agent and tool can read and write.
- **Handoff pipeline**: when an agent releases the lock with `--status complete`, the daemon verifies the work, commits it, and (if enabled) rebases, pushes and fast-forwards other branches. It stops at the first failing step and never ships paused or failed work.

---

## 🚀 Quickstart

Build (Go 1.22 or newer):

```bash
go build ./cmd/orchestra
```

On Windows this produces `orchestra.exe`. Or install it onto your `PATH`:

```bash
go install github.com/Adeel0-0/agent-orchestra/cmd/orchestra@latest
```

Then:

```bash
orchestra doctor      # which agents are installed and signed in
orchestra             # open the workspace in your browser
```

The workspace has projects and sessions, a streaming chat with approval buttons, agent and model selection, a diff of changed files, settings and light/dark themes. Type or paste the project folder path (there is no native folder picker yet). It runs on a private `127.0.0.1` port; Ctrl+C in the terminal stops it and the agents. Reloading the page needs a fresh launch, because the one-use bootstrap link has been spent. See [docs/WINDOW.md](docs/WINDOW.md).

The agent CLIs must already be installed and signed in:

| Agent (`-a`) | Account | Needs |
|---|---|---|
| `codex` (default) | ChatGPT Plus / Pro | `npm install -g @openai/codex`, then `codex login` |
| `antigravity` | Google AI Pro / Ultra | Antigravity CLI (`agy`), signed in once interactively |
| `claude` | Claude Pro / Max | Claude Code, signed in |
| `openrouter` | OpenRouter account | `orchestra openrouter-key`, then pick a model with `orchestra models` |

---

## 💬 Chat from the terminal

```bash
orchestra chat "explain this repo"                   # one turn with Codex in the current folder
orchestra chat -a claude -C ../app                   # a conversation with Claude Code in another folder
orchestra chat -r <session-id> "and now add tests"   # continue a previous session
orchestra chat -a openrouter -m <model-id>           # Orchestra's own agent loop on an OpenRouter model
```

`-p` sets what the agent may do: `read-only`, `edit` (default) or `full` (no limits). `--json` prints the event stream instead of formatted text. Ctrl+C stops the current turn and every process the agent started.

What `edit` means for each agent, checked against the real CLIs on 2026-11-21:

| Agent | `edit` uses | Edits files | Runs commands |
|---|---|---|---|
| Codex | `-s workspace-write` | yes | yes, in Codex's sandbox |
| Antigravity | `--sandbox` | yes | yes, in Antigravity's sandbox |
| Claude Code | `--permission-mode auto` | yes | yes, **not sandboxed**; Claude Code's own safety checker blocks risky actions (its sandbox is not available on Windows) |
| OpenRouter | Orchestra's own policy | yes | asks first |

`read-only` maps to each CLI's plan or read-only mode; `full` turns off every check. OpenRouter details: [docs/OPENROUTER.md](docs/OPENROUTER.md).

---

## 🛠️ Coordinating several agents

### 1. Initialize in any project

```bash
orchestra init
```
This writes a starter `orchestra.json`. Keys you leave out keep their defaults, and `workspace_root` is resolved relative to the folder `orchestra` searched. A file that cannot be parsed stops every command with an error naming it. Unknown keys are currently ignored (strict validation is not implemented). YAML configs are not supported.

### 2. Claim before editing
```bash
orchestra claim AUTH-001 --agent claude --scope "OAuth2 token rotation" --lease 1800
```
Emits `AGENT_CLAIM` and creates `.orchestra/writer.lock`. Other agents are refused until the lease expires or the lock is released.

### 3. Check the state
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
`orchestra status --json` prints the same as JSON.

### 4. Release and hand off
```bash
orchestra release --agent claude --status complete --summary "Implemented JWT rotation and 12 unit tests"
```
Emits `HANDOFF_REQUEST` with the status and the lock that was released, and removes the lock. The daemon ships a handoff only if it is `complete`, comes from the agent that held the lock, and arrives before the lease expired (`internal/policy`). `--status paused` and `--status failed` never ship; the daemon reports `PAUSED_CHECKPOINT` and leaves the work untouched.

### 5. Custom events and listening
```bash
orchestra emit REVIEW_VERDICT '{"task_id": "AUTH-001", "accepted": true}' --agent codex
orchestra listen
```
```text
[LISTEN] Listening to Agent Orchestra event bus (Ctrl+C to stop)...
[AGENT_CLAIM] (claude) -> {"scope": "OAuth2 token rotation", "task_id": "AUTH-001"}
[HANDOFF_REQUEST] (claude) -> {"status": "complete", "task_id": "AUTH-001"}
[VERIFICATION_STARTED] (orchestra) -> {"command": "python scripts/example_verifier.py"}
[GIT_COMMITTED] (orchestra) -> {"commit_message": "chore(orchestra): automated multi-agent handoff sync"}
```

### 6. Run the daemon
```bash
orchestra start --interval 2.0
```
The daemon:
1. Watches the configured paths for changes.
2. Reads new `HANDOFF_REQUEST` events, resuming from `.orchestra/daemon_cursor.json` after a restart. On its first start it skips existing history.
3. Runs the pipeline once per approved handoff, holding the writer lock as `orchestra` so no agent starts editing mid-pipeline. A dirty tree with no handoff is only reported.
4. Commits on `main_branch` when verification passes. With `auto_push` it then fetches, rebases, pushes and moves each `sync_branches` entry forward on the remote (`git push origin main:<branch>`, no checkout). The first failing step stops the sync: a failed rebase is aborted, `GIT_SYNC_FAILED` names the step, and the state goes to `ERROR` until the next writer claims.

### Pipeline commands

The pipeline runs shell commands you configure; the example scripts in `scripts/` are Python, but any command works:

```json
{
  "pipeline": {
    "verify_command": "python scripts/example_verifier.py",
    "post_sync_command": "python scripts/example_notifier.py",
    "on_error_command": null
  }
}
```

`orchestra run scripts/example_verifier.py` runs a script through the same runner (`.py` files use `python`, `python3` or `py -3` from `PATH`). Commands time out after 300 seconds; a timeout kills the command and everything it started. JSON printed by the command is captured in the `VERIFICATION_RESULT` event.

---

## 🧭 Local API

`orchestra serve` starts only the protected HTTP/WebSocket API on a random `127.0.0.1` port and prints its URL and a private launch token. See [docs/LOCAL-API.md](docs/LOCAL-API.md). Data lives in `~/.orchestra` (`--data-dir` picks another folder): an SQLite journal of every session plus readable JSONL transcripts.

---

## 🧪 Tests

```bash
go vet ./...
go test ./...
```

The UI (React, in `web/`) builds into `internal/webui/dist`, which the binary embeds:

```bash
cd web
npm ci
npm run build
npm test                 # unit tests
npx playwright test      # browser test against the Go fixture server (internal/app/uifixture)
```

Tests use throwaway repositories, recorded real CLI output and fake agents; they never use real API credentials or credits.

---

## 📜 License
MIT License. Created by Syed Mohammad Adeel.

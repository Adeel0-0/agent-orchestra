# OpenRouter terminal agent

Phase 2 supplies an agent loop for OpenRouter's public API. Subscription players
continue to use their official CLIs. OpenRouter uses separate API credits and a key
stored in the OS credential manager.

From this repository, in an interactive terminal:

```powershell
python -m pip install -e ".[openrouter]"
python -m orchestra openrouter-key
python -m orchestra models
python -m orchestra chat -a openrouter -m <model-id> -C <project-folder>
```

The hidden key prompt saves through `keyring` (Windows Credential Manager on
Windows). It refuses file-based backends and terminals that cannot hide input.
Keep keys out of command arguments, environment files, `orchestra.json` and
transcripts. Running `openrouter-key` again replaces the saved key. Core and CLI
players work without the optional dependency.

`models` reads the public catalog and lists only models advertising tool support.
`models --json` includes context and pricing metadata. Select a model explicitly;
there is no paid model default. `doctor` checks local key presence, not credits or
access to a specific model.

Add a prompt for one turn; omit it for a conversation in one process:

```powershell
python -m orchestra chat -a openrouter -m <model-id> -C <project-folder> "Add a test for the parser and run it"
```

The player emits the existing `session.started`, `text.delta`, `reasoning.delta`,
`tool.*`, `permission.requested`, `file.changed`, `usage`, `error` and
`turn.completed` events. Usage includes per-turn tokens/cost and accumulated
`session_input_tokens`, `session_output_tokens`, `session_cost_usd`. Only costs
returned by the API are known; a disconnected request may incur unreported charges.

The loop collects complete streamed tool calls, executes them in order, appends one
result per call and requests the next response. A turn stops after at most 50 model
steps. Token-limit stops, malformed calls and incomplete/error streams cannot run
partial tool requests. Requests are never retried automatically.

| Permission mode | File reads/search | File writes/edits | Commands |
|---|---|---|---|
| `read-only` | Automatic | Ask | Ask |
| `edit` (default) | Automatic | Automatic | Ask |
| `full` | Automatic | Automatic | Automatic |

`read-only` keeps the existing CLI flag name; for OpenRouter it means ask before
every change. Only explicit `y` or `yes` allows an action. A denial becomes a failed
tool result returned to the model. `--json` prints JSONL without questions and
automatically denies actions requiring approval. Non-interactive input also denies.
Future UI consumers can pause on `permission.requested` and call
`answer_permission(request_id, allow)` directly.

File tools resolve every accessed path against the project and reject escapes,
including external symlinks/junctions, and Windows device/alternate-stream paths.
Search skips links and common generated directories. Files are limited to 1 MiB
of UTF-8 text, displayed output to 16,000 characters and listings to 1,000 entries.
`edit_file` requires exactly one non-empty match; `grep` searches literal text.
These ordinary path checks do not protect against another process concurrently
changing the filesystem.

`read_file` has no offset/range option yet. For content beyond its display cap,
use targeted `grep` or an approved command. This remains a deferred enhancement.

**Commands are not sandboxed.** They start in the project with the normal access of
your OS account and can access other folders and the network. Review approvals
accordingly; choose `full` only when you intend to grant that access. Commands use
PowerShell on Windows and `/bin/sh` elsewhere, no stdin, a 30-second timeout and a
16,000-byte retained-output cap. Cancellation and timeout kill the active command
tree.

Project-root `AGENTS.md` and `CLAUDE.md` are included in system context. Near the
context limit, the same model summarizes older conversation (also using credits).
Tool-call/result groups stay together. Inputs too large to summarize safely fail
with handoff guidance instead of silently dropping history.

Standalone chat conversations/cost totals live in memory, and its OpenRouter
`--resume` remains explicitly rejected. The Phase 3 [local server](LOCAL-API.md)
persists history/usage in private session storage and resumes through session IDs.
No transcript or key is written to the project repository.
Ctrl+C stops a turn. Cancelled tool groups receive cancelled results in local
history so a following turn stays valid. Network closure is requested immediately;
a blocked urllib daemon worker can remain until its 30-second idle socket timeout.
No more tools run after cancellation.

## Verification

`python -m unittest discover tests` exercises synthetic documented API chunks and a
local HTTP server, plus real temporary files and subprocesses. Tests use no real
credentials or credits. API examples are separate from recorded CLI fixtures.
The symlink test skips on Windows without privileges; a separate junction test
exercises external-directory escape without those privileges.

Offline wrap-up completed on 2026-11-14. The latest full suite ran 98 tests in
41.805 seconds: 97 passed and 1 symlink-privilege test skipped. Final diff whitespace,
Python compilation, CLI help, doctor and public model-catalog smoke checks passed
after the review fixes. Those final checks required no implementation changes, so
the full suite was not repeated. The implementation is now published in PR #3 with
the annotated phase-3-offline milestone tag; live acceptance remains pending.

Live acceptance still needs owner setup: pick an inexpensive tool-capable model,
ask it to add a small test in a disposable project and run it, approve one command,
deny another, then inspect test results and the diff. This has not yet been run on
this machine.

Protocol references: [streaming](https://openrouter.ai/docs/api/reference/streaming),
[tool calling](https://openrouter.ai/docs/guides/features/tool-calling),
[usage accounting](https://openrouter.ai/docs/guides/guides/usage-accounting),
[model catalog](https://openrouter.ai/docs/api/api-reference/models/get-models).

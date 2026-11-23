# OpenRouter terminal agent

Phase 2 supplies an agent loop for OpenRouter's public API. Subscription players
continue to use their official CLIs. OpenRouter uses separate API credits and a key
stored in the OS credential manager.

From this repository, in an interactive terminal:

```powershell
orchestra openrouter-key
orchestra models
orchestra chat -a openrouter -m <model-id> -C <project-folder>
```

The hidden key prompt saves the key in the OS credential store: Windows Credential
Manager (in the same layout Python's `keyring` used, so an earlier key still works),
the macOS Keychain, or the Linux secret service. It refuses terminals that cannot
hide input.
Keep keys out of command arguments, environment files, `orchestra.json` and
transcripts. Running `openrouter-key` again replaces the saved key.

`models` reads the public catalog and lists only models advertising tool support.
The public catalog is cached for five minutes across clients in the server process.
`models --json` includes context and pricing metadata. Select a model explicitly;
there is no paid model default. `doctor` checks local key presence, not credits or
access to a specific model.

Add a prompt for one turn; omit it for a conversation in one process:

```powershell
orchestra chat -a openrouter -m <model-id> -C <project-folder> "Add a test for the parser and run it"
```

The player emits the existing `session.started`, `text.delta`, `reasoning.delta`,
`tool.*`, `permission.requested`, `file.changed`, `usage`, `error` and
`turn.completed` events. Usage includes per-turn tokens/cost and accumulated
`session_input_tokens`, `session_output_tokens`, `session_cost_usd`. Only costs
returned by the API are known; a disconnected request may incur unreported charges.

The loop collects complete streamed tool calls, executes them in order, appends one
result per call and requests the next response. A turn stops after at most 50 model
steps. Malformed calls and incomplete/error streams cannot run partial tool requests.
An output-limit stop discards every tool call from that response and asks the model
to continue or split the work into smaller complete edits. Output budgets use the
catalog model's completion ceiling, bounded by remaining estimated context, with an
8,192-token fallback when the catalog gives no ceiling. Summaries also respect the
ceiling. Repeated output-limit recovery still consumes the turn's step allowance.

HTTP 429/5xx failures get at most three total attempts with exponential or
Retry-After backoff capped at 30 seconds. No retry occurs after the first streamed
chunk, including tool fragments or reasoning. Cancellation interrupts backoff.
HTTP failures include the bounded `error.message` when supplied, with the actual API
key and Bearer credentials redacted; response bodies are never echoed wholesale.

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
Writes under `.git` and `.orchestra` are refused, including resolved aliases and
case/trailing-dot Windows path variants. Writes to existing hardlinked files are
also refused because an ordinary filename can alias protected metadata. Reads
remain available. Search skips links and common generated directories. Files are
limited to 1 MiB of UTF-8 text, output to 16,000 characters and listings to 1,000 entries.
`edit_file` requires exactly one non-empty match; `grep` searches literal text.
These ordinary path checks do not protect against another process concurrently
changing the filesystem.

`read_file` accepts zero-based line `offset` (default 0) and line `limit` (default
200, maximum 2,000). Partial/ranged results report remaining lines and the next
offset; fetch subsequent ranges to inspect a large file. A single line larger than
the output cap requires a targeted search or approved command. `glob`/`grep` stop
after 10,000 scanned files with an explicit scan-truncated marker, including when
no match was found; narrow `path` to continue searching.

**Commands are not sandboxed.** They start in the project with the normal access of
your OS account and can access other folders and the network. Review approvals
accordingly; choose `full` only when you intend to grant that access. Commands use
PowerShell on Windows and `/bin/sh` elsewhere, no stdin, a default 30-second timeout
and a 16,000-byte retained-output cap. `run_command.timeout` may extend the deadline
up to 600 seconds; that value appears in the approval input. Cancellation/timeout
kill the command tree. Windows PowerShell output is explicitly UTF-8. Variables
whose names end in `_KEY`, `_TOKEN` or `_SECRET` (case insensitive) are removed from
the child environment. This suffix filter is not complete secret isolation: an
approved command retains normal OS/file access, and other variable names may still
carry sensitive data.

Project-root `AGENTS.md` and `CLAUDE.md` are included in system context. Near the
context limit, the same model summarizes older conversation (also using credits).
Context estimates start at approximately four UTF-8 bytes per token with a guard
margin and calibrate from actual API `prompt_tokens`. Measurements survive saved
session restore. This is a heuristic, not an exact tokenizer or a guarantee for
every model/language.
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

`go test ./internal/openrouter/... ./internal/tools/...` exercises synthetic
documented API chunks and a local HTTP server, plus real temporary files and
subprocesses. Tests use no real
credentials or credits. API examples are separate from recorded CLI fixtures.
The symlink test skips on Windows without privileges; a separate junction test
exercises external-directory escape without those privileges.

History (Python version): offline wrap-up completed on 2026-11-14. The latest full suite ran 98 tests in
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

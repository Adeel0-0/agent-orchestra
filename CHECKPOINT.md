# Checkpoint

- **Status:** PAUSED after Phase 1. No writer active. Everything committed and pushed; PR #2 open.
- **Objective:** Turn Agent Orchestra into a simple self-owned coding-agent app: open from terminal or window, open a project, use ChatGPT Plus, Google AI Pro and OpenRouter models. Plan in `docs/PLAN.md`.
- **Branch:** `claude/youthful-rosalind-4b4ef5` (worktree `D:\agent-orchestra\.claude\worktrees\youthful-rosalind-4b4ef5`), with `origin/main` merged in. PR: https://github.com/Adeel0-0/agent-orchestra/pull/2 (base `main`).
- **How the engine stands (main's design, kept as is):**
  - PR #1 (merged to `main`) did Phase 0 its own way, and this branch now uses that version of `bus.py`, `daemon.py`, `config.py`, `git_sync.py`, `models.py`, `runner.py` unchanged.
  - The ship rule is `orchestra/policy.py::should_ship`: status complete, from the lock holder, within the lease.
  - Unknown config keys raise `ConfigError`. `ERROR` is held until the next writer claims. There is no `orchestra reset`.
  - This branch's own Phase 0 versions of those files were dropped in the merge. The only Phase 0 pieces kept from this branch: `orchestra/__main__.py`, the `if __name__ == "__main__"` guard in `cli.py`, and `ci_tracker._get_github_token` (env or `gh auth token`).
- **Phase 1 (this branch):** `orchestra/players/` (`base.py` event contract, `process.py` runner, Codex/Claude/Antigravity players, `registry.py`), `orchestra/chat.py`, `orchestra chat` and `orchestra doctor` in `cli.py`, tests in `tests/test_players.py`, `tests/test_entry_point.py`, `tests/fake_cli.py`, `tests/fixtures/`.
- **Design facts learned:**
  - `codex` on Windows is an npm `.CMD` shim, so prompts go on stdin; `process.py` refuses cmd.exe metacharacters in arguments to `.cmd`/`.bat`.
  - `codex exec resume` has no `-s`/`-C`: sandbox goes via `-c sandbox_mode=...`, folder via process cwd.
  - Codex wraps commands in `powershell.exe -Command '...'`; `display_command` unwraps it for display, `raw_command` keeps the original.
  - Antigravity docs: events `init`, `step_update`, `result`; resume `--conversation <id>`; no cwd flag; `--mode plan|accept-edits`; prompt passed as the `-p` value.
- **Owner decisions pending:**
  - `classify_error` in `orchestra/players/base.py` is a TODO the owner chose to write (error text to auth/quota/model/other). Returns "other" until then.
  - `auto_push` defaults to `true` in `config.py` and `orchestra.default.json`, as on `main`. Earlier work proposed `false`; the owner decides.
- **Checks run:** `python -m unittest discover tests`: 60 passed (about 35s). Earlier, before the merge: a real Codex edit turn and a resumed turn through `orchestra chat`, exit 0 both.
- **Not verified:** Antigravity end to end (not installed); real Ctrl+C in a console; `orchestra chat -a claude` end to end; interactive conversation mode by hand; Linux and macOS. The real Codex run happened before the merge, but the merge did not touch `orchestra/players/` or `chat.py`.
- **Running processes:** none.
- **Next steps:**
  1. If `agy` is installed and signed in: `python -m orchestra chat -a antigravity --json "say pong"`, save the raw CLI output as `tests/fixtures/agy_*.jsonl`, fix the guesses listed in `orchestra/players/antigravity.py`. If not, ask the owner; do not install it yourself.
  2. Phase 2: the OpenRouter agent loop (`docs/PLAN.md` section 7). It must emit the same events as `orchestra/players/base.py`.

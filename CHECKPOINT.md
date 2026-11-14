# Checkpoint

- **Status:** PAUSED after Phase 1. No writer active. Phase 1 is NOT committed.
- **Objective:** Turn Agent Orchestra into a simple self-owned coding-agent app: open from terminal or window, open a project, use ChatGPT Plus, Google AI Pro and OpenRouter models. Plan in `docs/PLAN.md`.
- **Branch:** `claude/youthful-rosalind-4b4ef5` (worktree `.claude/worktrees/youthful-rosalind-4b4ef5`). Base `4eba3d9` (Phase 0, committed). Phase 0 also exists on `claude/wonderful-wilson-2b14a9`.
- **Phase 1 files (uncommitted):**
  - new: `orchestra/players/{__init__,base,process,codex,claude,antigravity,registry}.py`, `orchestra/chat.py`, `tests/test_players.py`, `tests/fake_cli.py`, `tests/fixtures/{codex_edit,codex_resume,codex_error,claude_tool}.jsonl`
  - edited: `orchestra/cli.py` (`doctor`, `chat`), `README.md`, `docs/PLAN.md`, `CHECKPOINT.md`
- **Design facts learned:**
  - `codex` on Windows is an npm `.CMD` shim, so prompts go on stdin; `process.py` refuses cmd.exe metacharacters in arguments to `.cmd`/`.bat`.
  - `codex exec resume` has no `-s`/`-C`: sandbox goes via `-c sandbox_mode=...`, folder via process cwd.
  - Codex wraps every command in `powershell.exe -Command '...'`; `display_command` unwraps it for display, the raw command is kept as `raw_command`.
  - Antigravity docs: events `init`, `step_update`, `result`; resume `--conversation <id>`; no cwd flag; `--mode plan|accept-edits`; prompt passed as the `-p` value.
- **Owner decision pending:** `classify_error` in `orchestra/players/base.py` is a TODO left for the owner (maps error text to auth/quota/model/other). It returns "other" for everything until then.
- **Checks run:**
  - `python -m unittest discover tests`: 45 passed.
  - Real Codex via `orchestra chat` on a scratch project: edit turn changed the file and ran a command; a `--resume` turn remembered it. Both exit 0.
  - Real Claude: one stream-json capture recorded as a fixture (not through `orchestra chat`).
  - `orchestra doctor`: Codex ready, Claude ready, Antigravity not installed.
- **Not verified:**
  - Antigravity end to end (not installed). Unknowns: whether `-p` takes the prompt as its value as assumed, whether a DONE `agent_response` repeats text, which parameter of a write tool holds the path.
  - Real Ctrl+C in a console (cancellation is tested by cancelling the asyncio task, which kills the grandchild process).
  - `orchestra chat -a claude` end to end, and the interactive conversation mode (no prompt) by hand.
  - Linux and macOS.
- **Running processes:** none.
- **Next steps:**
  1. Owner installs Antigravity (`irm https://antigravity.google/cli/install.ps1 | iex` in PowerShell), runs `agy` once to sign in with the AI Pro account, then: `python -m orchestra chat -a antigravity --json "say pong"` and save real output to `tests/fixtures/agy_*.jsonl`.
  2. Owner fills in `classify_error`.
  3. Commit Phase 1, then start Phase 2 (OpenRouter agent loop).

Pickup prompt: "Read CHECKPOINT.md and docs/PLAN.md, check git status, then continue from Next steps."

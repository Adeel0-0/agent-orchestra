# Checkpoint

- **Status:** PAUSED after Phase 0. No writer active. Nothing committed.
- **Objective:** Turn Agent Orchestra into a simple self-owned coding-agent app: open from terminal or window, open a project, use ChatGPT Plus, Google AI Pro and OpenRouter models. Plan in `docs/PLAN.md`.
- **Branch:** `claude/wonderful-wilson-2b14a9`, base and current commit `303bc85`.
- **Changed files (uncommitted):**
  - new: `orchestra/__main__.py`, `tests/test_phase0.py`, `docs/PLAN.md`, `CHECKPOINT.md`
  - edited: `orchestra/bus.py`, `cli.py`, `config.py`, `daemon.py`, `git_sync.py`, `ci_tracker.py`, `models.py`, `runner.py`, `orchestra.default.json`, `README.md`
- **Phase 0 done:**
  - `python -m orchestra` works; importing `orchestra.cli` no longer runs it; new `orchestra reset`.
  - Writer lock is serialized by an `O_EXCL` guard file (`writer.lock.guard`).
  - Event log: `read_from(offset)`, tail reads, rotation to `events.1.jsonl` at 5 MB.
  - Daemon runs the pipeline once per `HANDOFF_REQUEST` with status complete (policy in `should_run_pipeline`, `orchestra/daemon.py`), holds `ERROR`, persists state in `.orchestra/daemon_state.json`.
  - Git sync stops at the first failing step, aborts a failed rebase, pushes only with `auto_push` (now default false) and only from the main branch; `fast_forward_branch` no longer checks out other branches.
  - CI token comes from env or `gh auth token`; child processes get no stdin.
  - Config sections ignore unknown keys.
- **Phase 0 not done:** streaming runner (moved to Phase 1 step 1, `players/process.py`). `CiTracker` is still not wired into the daemon. `orchestra.sefgh.json` still has `auto_push: true` (left as the owner's explicit setting).
- **Checks run:** `python -m unittest discover tests` gives 25 passed. Lock race test repeated 6 times, all passed. `python -m orchestra status` and `reset` run.
- **Checks not run:** push, rebase and CI paths against a real remote; the daemon loop (`orchestra start`) end to end; anything on Linux or macOS.
- **Not verified:** That Google AI Pro works through `agy` headless mode on this account. Exact JSON event shapes of `codex exec --json` and `agy` stream-json.
- **Awaiting owner:** Whether to commit Phase 0, and whether to begin Phase 1.
- **Running processes:** none.
- **Next step:** Phase 1 step 1 in `docs/PLAN.md`: create `orchestra/players/process.py`, then capture `codex exec --json -s read-only "list the files in this repo"` into `tests/fixtures/`.

Pickup prompt: "Read CHECKPOINT.md and docs/PLAN.md, check git status, then start Phase 1."

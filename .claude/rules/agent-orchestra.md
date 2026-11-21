# Agent Orchestra project rules (override ECC)

These rules are specific to this repository. Where they conflict with the
vendored ECC rules in `.claude/rules/ecc/`, **this file wins**. ECC is a
general toolkit; Agent Orchestra has its own design and owner decisions.

## Read first

- `CHECKPOINT.md` is the durable task ledger and recovery record. Read it
  before editing, follow its ownership and owner-decision notes, and update it
  after each bounded milestone. ECC's own `/checkpoint` command is not
  installed here on purpose.
- `docs/PLAN.md` is the product plan. Use `/ecc-plan` or the `planner` agent
  to plan *within* it, not to replace it.

## Dependencies

- The core package (`orchestra/`) uses the Python 3.10+ standard library only.
  Optional extras live in `pyproject.toml` (`watch`, `yaml`, `openrouter`,
  `app`, `test-app`).
- Do not add a dependency, a dev tool config, or a `requirements*.txt` without
  the owner's OK. This applies to the tools ECC names (pytest, black, isort,
  ruff, mypy, bandit, python-dotenv). Use them only if they are already
  installed, and never make a check depend on them.
- ECC's "search registries and adopt a library first" step does not apply to
  the core engine. Mention a candidate library to the owner instead of adding
  it.

## Tests and verification

- The test framework is **unittest**, not pytest. Use ECC's `python-testing`
  skill for test design ideas only, and write tests as `unittest.TestCase`.
- Full suite: `python -m unittest discover tests`
- One file: `python -m unittest discover tests -p test_<name>.py`
- App tests need the `test-app` extra. Expect one symlink-privilege skip on
  Windows.
- Test first (RED, then GREEN) as ECC asks. Coverage: there is no coverage
  dependency. Run `python -m coverage` only if it is already installed, and
  treat 80% as a target, not a gate.
- When the `verification-loop` skill shows npm/tsc/eslint commands, use these
  instead: `python -m compileall -q orchestra`, the unittest suite above, and
  `git diff --check`.

## Secrets

- OpenRouter keys live in the OS keyring (`keyring`). Never store keys in the
  repository, in `.env` files, or in CLI arguments. Never print a key.
  Ignore ECC's `python-dotenv` example.
- `ci_tracker` gets a GitHub token from the environment or `gh auth token`.
  Never log it.

## Git

- Do not commit, push, rebase, reset, force-push or stash unless the owner
  asks. Ignore ECC's "Commit & Push" step as an automatic step.
- Conventional commit types from ECC are fine (`feat:`, `fix:`, ...).
- Ignore ECC's note about `includeCoAuthoredBy`. Attribution follows the
  harness and the owner's settings.

## Agents and commands

- Agents are installed here with bare names: `planner`, `architect`,
  `code-reviewer`, `python-reviewer`, `security-reviewer`, `tdd-guide`,
  `silent-failure-hunter`. They are **not** `ecc:`-prefixed. Agents that the
  ECC rules mention but that are not installed (for example `e2e-runner`,
  `build-error-resolver`, `doc-updater`) do not exist in this project.
- ECC says to run agents "proactively" or "immediately". Here that is a
  suggestion. Spawn a subagent when the owner asks for one or when the
  harness instructions allow it.
- Commands are prefixed `ecc-` so they don't collide with built-in commands:
  `/ecc-plan` and `/ecc-python-review`. For reviews, the built-in
  `/code-review` also works. The vendored files still mention their upstream
  names (`/plan`, `/python-review`) and some ECC commands that are not
  installed here (`/multi-plan`, `/plan-canvas`, `/plan-prd`, `/prp-*`,
  `/tdd`). Treat those mentions as absent.
- Run the `security-reviewer` agent for changes to `orchestra/tools/`
  (filesystem and shell tools), `orchestra/players/process.py` (subprocess
  and cmd.exe argument handling), the OpenRouter client and credentials, and
  the local app server in `orchestra/app/`.

## Style notes

- ECC's immutability rule applies to new code. Do not refactor existing
  engine dataclasses just to make them frozen.
- `print()` is fine in `orchestra/cli.py` and `orchestra/chat.py`, where it is
  the user interface. Use `logging` elsewhere.
- This is a Windows-first project that also runs on Linux and macOS. Use
  `pathlib`, and never build a shell string from user input. See the cmd.exe
  metacharacter guard in `orchestra/players/process.py`.

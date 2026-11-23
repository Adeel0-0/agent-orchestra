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

- The backend is Go (module `github.com/Adeel0-0/agent-orchestra`, `go 1.22` in
  go.mod). The Python implementation was removed on 2026-11-23 after the Go port
  reached parity; `scripts/*.py` are only example pipeline hooks.
- Approved modules (owner, 2026-11-23): the Go standard library,
  `golang.org/x/sys`, `github.com/fsnotify/fsnotify`, `modernc.org/sqlite`,
  `github.com/coder/websocket`, `github.com/zalando/go-keyring`. Pin versions that
  keep `go 1.22`. Do not add any other module (including `golang.org/x/term` or
  helpers that only appear as indirect dependencies) without the owner's OK.
- The UI in `web/` (React/TypeScript) keeps its own npm dependencies; ask before
  adding new ones there too.
- ECC's "search registries and adopt a library first" step does not apply to
  the core engine. Mention a candidate library to the owner instead of adding
  it.

## Tests and verification

- Tests use Go's `testing` package. Use ECC's testing skills for test design ideas
  only.
- Full check: `go vet ./...` and `go test ./...`. Also run `GOOS=linux go vet ./...`
  and `GOOS=darwin go vet ./...`: the project is Windows-first but must build on
  Linux and macOS. Expect a few symlink-privilege skips on Windows.
- UI: `cd web; npm run build` (outputs to `internal/webui/dist`, which the binary
  embeds), `npm test`, and `npx playwright test` (uses the Go fixture server
  `internal/app/uifixture`).
- Test first (RED, then GREEN) as ECC asks. Coverage is a target, not a gate.
- When the `verification-loop` skill shows npm/tsc/eslint commands for the
  backend, use `go vet ./...`, `go test ./...` and `git diff --check` instead.

## Secrets

- OpenRouter keys live in the OS keyring (`keyring`). Never store keys in the
  repository, in `.env` files, or in CLI arguments. Never print a key.
  Ignore ECC's `python-dotenv` example.
- Never log the app server's launch token, and never put it in a URL.

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
- Run the `security-reviewer` agent for changes to `internal/tools/`
  (filesystem and shell tools), `internal/runner/` (process groups, stream.go
  and the cmd.exe argument guard), `internal/openrouter/` (client and
  credentials), and the local app server in `internal/app/`.

## Style notes

- ECC's immutability rule applies to new code where it fits Go; do not refactor
  existing code just for it.
- Printing to stdout/stderr is fine in `cmd/orchestra` and `internal/chat`, where
  it is the user interface. Elsewhere return errors or events instead.
- This is a Windows-first project that also runs on Linux and macOS. Use
  `path/filepath`, put OS-specific code behind build tags, and never build a shell
  string from user input. Prompts go to agent CLIs on stdin; see the cmd.exe
  metacharacter guard in `internal/runner/stream.go`.

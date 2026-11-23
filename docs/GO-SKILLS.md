# Go skills in Agent Orchestra

This project vendors a subset of
[samber/cc-skills-golang](https://github.com/samber/cc-skills-golang) into
`.claude/skills/`, so every Claude Code session in this repository gets the same
Go guidance while working on the Go port (`cmd/orchestra`, `internal/`). Nothing
is installed globally, and no plugin is needed.

- **Source:** `samber/cc-skills-golang`, commit
  `d68d80aec298e1b74b37c2a472c002b5217248b3` (2026-11-23).
- **License:** MIT. The copyright notice is in `.claude/CC-SKILLS-GOLANG-LICENSE`.
- **Changes from upstream:** the `evals/` folders were left out. All other
  files are copied unchanged. Project-specific changes live in
  `.claude/rules/agent-orchestra.md` (section "Go port"), which overrides the
  skills wherever they conflict. Most importantly, it says stdlib first and
  no new Go modules.

## What is installed

| Skill | Why it is here |
|-------|----------------|
| `golang-concurrency` | daemon, bus and watcher goroutines, channels, shutdown |
| `golang-context` | cancellation and timeouts for runs |
| `golang-error-handling` | wrapping, `errors.Is/As`, `log/slog` |
| `golang-testing` | table-driven tests and the cross-language tests |
| `golang-safety` | nil, slice aliasing and integer conversion pitfalls |
| `golang-security` | process launching, paths, secrets (references) |
| `golang-troubleshooting` | races, deadlocks, hangs, leaked processes |
| `golang-code-style` | readability rules that linters can't check |
| `golang-naming` | identifiers, packages and errors |
| `golang-lint` | what golangci-lint checks (its `.golangci.yml` is a template only) |

## What was left out, and why

- **`golang-cli`.** Its default is Cobra + Viper, and its examples carry their
  own `go.mod`. This CLI uses only the standard library and has to match the
  Python CLI's output.
- **`golang-project-layout`.** It is for starting a new project. One of its
  checklist steps writes into `CLAUDE.md` "no user confirmation needed", and
  its assets include a real `.gitignore` that would be active inside
  `.claude/`.
- **Library skills** (`golang-samber-*`, `golang-uber-*`, `golang-google-wire`,
  `golang-spf13-*`, `golang-stretchr-testify`) and **stack skills** (gRPC,
  GraphQL, Swagger, database, observability). This code uses none of them.
- **Everything else** (performance, benchmark, design-patterns, refactoring,
  gopls, modernize and others). Add it if the work needs it.

## Updating

```powershell
git clone --depth 1 https://github.com/samber/cc-skills-golang.git $env:TEMP\ccsg-src
```

Diff each vendored skill folder against `skills/<name>/` upstream. Read the
changes before you copy them in: these files become instructions for every
agent that runs here. Look especially for new `go get` / `go install` steps,
or steps that write to agent config files. Leave out `evals/`, update the
commit hash above, and keep the override section in
`.claude/rules/agent-orchestra.md`.

## Global alternative

To get all 46 skills in every project instead, the owner can run
`/plugin marketplace add samber/cc`, then
`/plugin install cc-skills-golang@samber`. The project override still
applies here.

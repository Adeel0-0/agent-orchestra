# ECC in Agent Orchestra

This project vendors a small, curated part of
[ECC (Everything Claude Code)](https://github.com/affaan-m/ECC) into `.claude/`,
so every Claude Code session opened in this repository gets the same planning
and review agents, rules and skills. Nothing is installed globally, and no
plugin is required.

- **Source:** `affaan-m/ECC`, commit `4adeee6732a12d1540f49a4d50a38ce8c97f9a53`
  (2026-11-19), README version 2.2.3.
- **License:** MIT. The copyright notice is in `.claude/ECC-LICENSE`.
- **Changes from upstream:** two commands were renamed (see below). All other
  files are copied unchanged. Project-specific changes live in
  `.claude/rules/agent-orchestra.md`, which overrides ECC wherever they
  conflict.

## What is installed

| Kind | Path | Items |
|------|------|-------|
| Rules | `.claude/rules/ecc/common/` | agents, code-review, coding-style, development-workflow, git-workflow, hooks, patterns, performance, security, testing |
| Rules | `.claude/rules/ecc/python/` | coding-style, hooks, patterns, security, testing (apply to `*.py`) |
| Project override | `.claude/rules/agent-orchestra.md` | stdlib-only core, unittest, keyring, no automatic git actions, CHECKPOINT.md |
| Agents | `.claude/agents/` | planner, architect, code-reviewer, python-reviewer, security-reviewer, tdd-guide, silent-failure-hunter |
| Skills | `.claude/skills/` | python-patterns, python-testing, security-review, verification-loop |
| Commands | `.claude/commands/` | `/ecc-plan` (upstream `plan.md`), `/ecc-python-review` (upstream `python-review.md`) |

## What was left out, and why

- **Hooks and `hooks.json`.** ECC's hooks run Node scripts from the ECC
  checkout on every session and tool call. This repo is Python-only, and
  hooks run without being asked, so they should be an explicit owner choice.
- **TypeScript and frontend agents and skills** (build-error-resolver,
  refactor-cleaner, doc-updater, e2e-runner, tdd-workflow, coding-standards).
  They are written around npm, tsc, eslint, knip and Playwright.
- **`search-first`.** It pushes toward adopting new packages, which goes
  against the zero-dependency core.
- **ECC's `/code-review`.** It has the same name as the built-in command, and
  its PR mode can post `gh pr review --approve`. Use the built-in
  `/code-review` instead.
- **`/checkpoint`, `/quality-gate`, `/test-coverage`, `/build-fix`.** The
  first two need ECC's hook scripts, and `CHECKPOINT.md` already does that
  job. The last two are written for JS tooling.
- **`rules/python/fastapi.md`.** The app uses Starlette, not FastAPI.
- **MCP configs, ECC2 dashboard, multi-model `/multi-*` commands.** They need
  external runtimes.

## Updating

```powershell
git clone --depth 1 https://github.com/affaan-m/ECC.git $env:TEMP\ecc-src
```

Diff each vendored file against the matching upstream file. Read the
changes before you copy them in: these files become instructions for every
agent that runs here. Then update the commit hash above. Keep the `ecc-`
prefix on commands, and keep the override file.

## Global alternative

If you want all of ECC in every project, ECC's own installer does that. It is
interactive: `npx ecc-universal@2.2.3 setup`. Do not combine it with this
project-local copy. The same agents would be loaded twice, under different
names.

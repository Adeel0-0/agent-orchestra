# The local coding-agent window (Phase 4)

Install `.[desktop]` and run `python -m orchestra` or `orchestra ui`. A private
pywebview window opens on a random IPv4 loopback port. Without native support the
launcher falls back to the default browser; `orchestra ui --browser` selects that
mode explicitly. Install `.[app]` for browser-only use. Wheels bundle the frontend;
a source checkout needs `cd web; npm ci; npm run build` first.

Use **Open folder** to select a project. Native mode opens the OS folder dialog;
browser mode asks for a full local path. Choose an installed agent and, for
OpenRouter, a tools-capable model. Start a session or type your first prompt. The
left rail keeps projects and durable sessions, the center streams text/tool cards
and inline approvals, and the right pane shows tracked diffs against the session's
starting commit. Untracked filenames are listed but their contents are not loaded.
**Allow edit / Allow command** grants one action; **Deny** rejects it. **Stop**
cancels a turn. Selecting another agent or model starts a fresh session on send.

Settings provide appearance, default agent and default permissions. API keys are
never entered into this window: use `python -m orchestra openrouter-key` privately
in a terminal after installing `.[openrouter]`. Credit/model access still needs
live acceptance. The first-run health check reports available CLIs and setup hints.

| Shortcut | Action |
| --- | --- |
| Ctrl+N | New session |
| Ctrl+K | Switch project |
| Ctrl+Shift+P | Focus agent selection |
| Ctrl+, | Settings |
| Escape | Close a dialog, or stop a running turn |
| Enter / Shift+Enter | Send / newline |

macOS command-key equivalents are accepted by the frontend, but macOS/Linux native
acceptance remains pending. Themes follow the OS by default and can be set to
light/dark. Side panels can be toggled on narrower windows.

The launcher passes a separate one-use, 60-second nonce in a URL fragment. The page
removes it immediately and exchanges it for an API token held only in page memory.
Neither token is stored in browser storage or cookies. Reloading/new tabs require
a fresh launcher window; the reconnect screen explains this. APIs retain exact
Host/Origin and Bearer/WebSocket-token checks. See [bootstrap details](PHASE4-BOOTSTRAP.md).

Closing the native window shuts down the server and cancels owned agents. Browser
mode keeps the server in its terminal until Ctrl+C. A slow native shutdown may
report that cleanup is still running; the non-daemon server thread keeps the
process alive until cleanup completes. Failed process cleanup still requires the
existing inspect/stop/recover confirmation; dismissing a window is not evidence
that every external process stopped.

This phase uses the selected folder directly. Isolated worktrees, commit/discard,
merge and PR controls belong to Phase 5 and are not included. Commands with
approval/full access remain ordinary OS commands, not an operating-system sandbox.

## Offline verification

```powershell
python -m pip install -e ".[test-app,desktop]"
python -m unittest discover tests
cd web
npm ci
npm run build
npm test
npm run test:e2e
```

The browser test uses installed Edge headlessly, a temporary Git project and an
offline fake player over real HTTP/WebSocket. It covers cold start, folder
selection, prompt/approval/edit/diff, a new model session, denial, reconnect/replay,
delayed old-session diff rejection, themes and narrow layout. Its fixture must
exit cleanly. Backend tests cover nonce expiry/replay/concurrent consumption,
Origin/body limits, public-shell boundaries and native-picker authentication.
Runtime tests start and stop a real server; packaged-wheel checks run outside the
source tree. No paid model or subscription run is part of these checks.

Real provider/keyring acceptance, manual console Ctrl+C, abrupt real-CLI crashes
and Linux/macOS acceptance remain separate. Exact results and native smoke details
are recorded in `CHECKPOINT.md`.

# The local coding-agent workspace (Phase 4)

Run `orchestra` (or `orchestra ui`). The server starts on a random IPv4 loopback
port and the workspace opens in your default browser. The built UI is embedded in
the Go binary; after changing `web/`, rebuild it with `cd web; npm ci; npm run build`
(output goes to `internal/webui/dist`) and rebuild the binary. There is no native
window yet (the Python version had one through pywebview); `--browser` is accepted
and changes nothing.

Use **Open folder** to select a project and enter its full local path (there is no
native folder dialog yet). Choose an installed agent and, for
OpenRouter, a tools-capable model. Start a session or type your first prompt. The
left rail keeps projects and durable sessions, the center streams text/tool cards
and inline approvals, and the right pane shows tracked diffs against the session's
starting commit. Untracked filenames are listed but their contents are not loaded.
**Allow edit / Allow command** grants one action; **Deny** rejects it. **Stop**
cancels a turn. Selecting another agent or model starts a fresh session on send.

Settings provide appearance, default agent and default permissions. API keys are
never entered into this window: use `orchestra openrouter-key` privately in a
terminal. Credit/model access still needs
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
a fresh launch; the reconnect screen explains this. If no browser can be opened,
the one-use address is printed in the terminal instead. APIs retain exact
Host/Origin and Bearer/WebSocket-token checks. See [bootstrap details](PHASE4-BOOTSTRAP.md).

The server keeps running in its terminal until Ctrl+C, which cancels running
turns and kills their process trees before exiting. Closing the browser tab does
not stop it. Failed process cleanup still requires the existing
inspect/stop/recover confirmation.

This phase uses the selected folder directly. Isolated worktrees, commit/discard,
merge and PR controls belong to Phase 5 and are not included. Commands with
approval/full access remain ordinary OS commands, not an operating-system sandbox.

## Offline verification

```powershell
go test ./...
cd web
npm ci
npm run build
npm test
npm run test:e2e
```

The browser test starts the Go fixture server (`go run ./internal/app/uifixture`)
and uses installed Edge headlessly, a temporary Git project and an offline fake
player over real HTTP/WebSocket. It covers cold start, folder
selection, prompt/approval/edit/diff, a new model session, denial, reconnect/replay,
delayed old-session diff rejection, themes and narrow layout. Its fixture must
exit cleanly. Backend tests cover nonce expiry/replay/concurrent consumption,
Origin/body limits and public-shell boundaries. Runtime tests start and stop a real
server. No paid model or subscription run is part of these checks. The browser test
passed against the Go server on 2026-11-23.

Real provider/keyring acceptance, manual console Ctrl+C, abrupt real-CLI crashes
and Linux/macOS acceptance remain separate. Exact results and native smoke details
are recorded in `CHECKPOINT.md`.

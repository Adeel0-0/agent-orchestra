# Phase 4: the window

Owner authorized starting Phase 4 on 2026-11-15. Branch codex/phase-4-window is
stacked on review fixes 175eeec / PR #4 (unmerged); do not mix new UI work into that
PR. Current checkpoints/ownership are in CHECKPOINT.md. Live providers stay pending.

Offline implementation/verification completed locally on 2026-11-15: 156 Python
tests (155 passed, one Windows symlink-permission skip), four frontend unit tests,
real HTTP/WS fake-provider browser acceptance, native window/folder-picker smoke,
packaged assets/runtime smoke, and independent review. Published as implementation
09ed7d5 in PR #5, based on PR #4's branch; neither PR has been merged.
See the final Phase 4 checkpoint for exact results, native smoke limitations and
the preserved owner-created test history.

## Architecture and milestones

1. Secure static shell + one-time bootstrap. Implement docs/PHASE4-BOOTSTRAP.md:
   exact-host public static assets, nonce exchange requiring local Origin, no
   unauthenticated API/data, CSP, no persistent tokens. Add replay/expiry/origin/
   duplicate-exchange regressions and native-picker capability endpoints.
2. React + Vite + TypeScript frontend, built into orchestra/app/web/dist. Implement
   typed HTTP/WebSocket client, project/session navigation, replay/dedup/reconnect,
   transcript/tool cards/reasoning/permissions, composer/player/model/permission
   controls, diff pane, first-run health and settings, themes and shortcuts.
3. Launcher/runtime: no-args orchestra and orchestra ui open pywebview; --browser
   uses default browser. Keep serve as API-only foreground command. Background
   runtime uses random loopback socket and closes with native window; native folder
   dialog is invoked via authenticated HTTP callback, not an exposed JS bridge.
   Browser mode supports manual folder paths. Native-window dependency optional.
4. Browser acceptance with fake backend over real HTTP/WS: cold start, add a
   project, submit prompt, approve edit, see diff, choose another model/session,
   reconnect. Verify light/dark/responsive/focus and packaged assets. Native-window
   smoke if available; no real provider quota. Run full tests/build/package/review.

## Visual direction

Subject: a private coding conversation with inspectable actions and changed files.
Audience: the owner working in local projects, needing low-friction agent switching.
Three columns are dictated by the existing plan, not a generic dashboard layout.

- Palette: canvas #f2f5f9, navigation #e8edf3, transcript #ffffff, ink #223047,
  action #315ab4, completed #126e68; amber approval #995b17. Dark counterpart uses
  slate #182230/#202d3d and readable pale text; no neon or gradient decoration.
- Type: native Segoe UI Variable/system UI; Cascadia Code/consolas for code/diffs.
- Layout: 248px project/session rail, flexible conversation, 330px diff; composer
  anchored to conversation bottom. Header shows project/session and actual status.
  Side panels collapse on smaller viewports rather than crushing the transcript.
- Signature: conversation timeline with clear action/approval cards and a quiet
  context rail; all visible data comes from the real API, never invented metrics.
- Accessibility: labeled controls, keyboard focus, reduced motion, no color-only
  status, live stream announcements restrained. No external scripts/fonts.

This follows the approved simple app shape. Distinct-artifacts guidance is applied
through this local design record; no global memory is written without owner request.
No marketing hero, equal-card dashboard, F1 portfolio styling or unrelated project
design is imported. Phase 5 worktrees/commit/discard/merge controls remain out of scope.

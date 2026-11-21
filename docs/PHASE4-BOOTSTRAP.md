# Phase 4 browser bootstrap decision

Implemented for the Phase 4 window and browser launcher. The API-only `serve`
command keeps its original authenticated route boundary. Only a UI runtime enables
the inert public shell, static assets and nonce exchange described here. Project,
session and action APIs remain protected by launch-token authentication and exact
Host/Origin checks.

A browser's initial page navigation cannot carry an Authorization header. Phase 4
serves only an inert static HTML/JS shell without API authentication, still
requiring the exact local Host. No projects, transcripts, credentials, action routes
or launch token can be embedded in that public shell or static assets.

The launcher generates a separate high-entropy, one-use bootstrap nonce with a
short expiry (60 seconds) and open the local page with `#bootstrap=<nonce>`. The
fragment never reaches the HTTP server. Page code must immediately remove it with
`history.replaceState`, then exchange the nonce in a JSON POST to a dedicated
bootstrap route. That one route requires the exact local browser Origin and Host,
strict body limits, an unexpired nonce and atomic single use; it is not a general
unauthenticated settings/API endpoint. Never accept bootstrap/API tokens in query
strings or log them.

The exchange returns the launch API token to page memory only. Subsequent API calls
use Authorization; WebSocket connections use the existing token subprotocol.
Do not store tokens in localStorage, sessionStorage, browser-visible generated
files or persistent cookies. Reconnects retain the page's in-memory token. A page
reload/new window must return through the launcher for a fresh bootstrap nonce;
the UI must make that recovery path clear rather than broadening API access.

The static shell uses a restrictive CSP and self-hosted assets, with no third
party scripts. Browser XSS/local malware are not solved by the nonce; keep all
existing API/Origin protections. Nonce replay, expiry, foreign Origin, double
exchange, refresh recovery and token removal are covered by backend/browser tests.
Reloading shows a reconnect screen: relaunch `orchestra ui` for a fresh window and
nonce. Appearance is the only localStorage setting; credentials are not persisted.

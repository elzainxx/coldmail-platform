# Phase 6 — User app: spec

Roadmap: [`coldmail-platform-build-plan.md`](coldmail-platform-build-plan.md). Previous: [`phase-5-spec.md`](phase-5-spec.md) (platform code `b010245`; verify `8aa147c`, send-managed `0065533`, send-byo `67a7e15`). One phase, one session, one report. Evidence is run ids; the verified head and its runs are cited here when the phase ends.

**Verified head:** _filled at the end (all eight workflows on one SHA, then the `e2e` scope on it)._

## Chunks

| # | Chunk | Exit check | Status | Evidence |
|---|---|---|---|---|
| A1 | Foundation: the design tokens file, light and dark, the component set, shell and navigation, a small router, the API client, the session (invite, sign in, forgot, sign out; tokens in memory, silent refresh), the copy catalogue and the error-to-wording map | web unit tests, typecheck, build | OPEN | — |
| A2 | The Go service serves the built app (`PLATFORM_WEB_DIR`): SPA fallback, `/config.json` (public Supabase URL and anon key), the strict CSP and `no-store` headers; the Dockerfile builds the web app | go unit tests, e2e | OPEN | — |
| A3 | API additions: `GET /v1/me`, `GET /v1/balances`, jobs `status` filter, `POST /v1/templates/check`, CSV `email_column`; each with a cross-user paired test where it reads user data | db-tests | OPEN | — |
| A4 | Suppressions (list, search, add, remove with its history, CSV import; migration) and the module 3 account routes (list, add, test, bulk, remove; user wording only) | db-tests | OPEN | — |
| A5 | Screens: dashboard; jobs table (polling, partial results), job detail, pause, resume, cancel, results pages, CSV download | web tests, e2e | OPEN | — |
| A6 | Screens: new verify job (CSV, column picker, preview, confirm, schedule); new send job (list from a CSV or a finished verify job, the authoring screen with inline grammar errors, samples, sandboxed preview) | web tests, e2e | OPEN | — |
| A7 | Screens: module 3 accounts; suppression list; settings (vendor opt-out with its disclosure, password) | web tests, e2e | OPEN | — |
| A8 | The copy gate: every API error code and reason has a user-wording entry, no internal text reaches a screen | `gates` (selftest with reasons) | OPEN | — |
| A9 | The `e2e` scope: GoTrue, the platform, modules 1, 2, 3 with workers, the sink and fixtures, a digest-pinned Playwright runner inside the private network, screenshots uploaded as artifacts | `harness` scope `e2e` | OPEN | — |
| A10 | End to end: invite and sign in; verify from CSV and download; chained send through module 2 to the sink with an unsubscribe link, click, next job excludes the address; module 3 account add with the From-gate result, test, send; pause, resume, cancel; vendor opt-out | `e2e` | OPEN | — |
| A11 | Cross-user (UI and API), the sandboxed preview runs no script, the DOM holds no internal text, keyboard reach, labels, contrast (automated) | `e2e` | OPEN | — |
| A12 | Phase 5 follow-up (owner ruling): at 7 days the template and the merge data are cleared with the rendered content; address, outcome, charge, job name, dates, counts kept | db-tests | OPEN | — |
| A13 | One review pass over the new endpoints, the serving and the preview sandbox | findings fixed | OPEN | — |
| A14 | Final verification (eight workflows once on the final code head, then the `e2e` scope on it), build plan P6 done, parent re-pinned | `check-verified-heads` | OPEN | — |

## Decisions taken (one line each; none is irreversible unless it says so)

1. **One origin.** The Go service serves the built app and the API from one origin (`PLATFORM_WEB_DIR`, off when unset): no CORS, no second server; the public edge of P8 proxies one upstream. The strict CSP is set by that Go code (one place, tested): `default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' https: data:; font-src 'self'; connect-src 'self' <the Supabase origin>; frame-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'`. No inline script anywhere. Inline styles are allowed because the sandboxed preview of a user's HTML needs its own (a document in `srcdoc` inherits the embedder's CSP).
2. **Login talks to GoTrue directly from the browser** with a small hand-written client (password sign in, refresh, recover, set password from an invite link, sign out); the only key the browser holds is the public anon key, delivered by `/config.json` from `PLATFORM_PUBLIC_SUPABASE_URL` and `PLATFORM_PUBLIC_SUPABASE_ANON_KEY`. Access and refresh tokens live in memory only (never storage), are refreshed silently before they expire, and a page reload asks for a sign in again. After sign out the state is gone, the document is `no-store`, and the back button shows the sign in page.
3. **No new dependency without a reason:** a hand-written router and fetch client, system fonts, no UI kit, no state library. Test-only additions: Playwright and an accessibility checker.
4. **User HTML is shown only in an iframe with `sandbox=""`** (no scripts, no same origin, no forms, no popups) through `srcdoc`; the app's own CSP also forbids inline script, so a `<script>` in a template runs under neither.
5. **One copy catalogue** (`web/src/copy/`): every string a user reads, and one wording per API error code and per `not_allowed` reason; an unknown code shows one generic line and is logged to the console, never shown raw. The server message of a module is never shown. The grammar messages of `internal/render` (a position and a plain description of the user's own template) are the one server text that is shown.
6. **CSV upload** is streamed from the file; the browser parses only the first rows for the column picker and the API takes the chosen column (`email_column`, additive: the other columns, including one named `email`, become merge fields).
7. **Suppression removal is allowed (owner brief) and recorded:** every removal writes an append-only history row (who, which entry, its source); the UI asks for a confirmation and says plainly that removing an address that unsubscribed itself makes the user responsible for mailing it again. Recommendation for the owner's report: keep this, or refuse removal of entries whose source is `unsubscribe`.
8. **Routes added are POST-style for removal** (`POST /v1/suppressions/remove`, `POST /v1/m3/accounts/{id}/remove`): addresses in a path are a hazard and the route-table gate reads `GET|POST|PUT`.
9. **The `e2e` scope** runs the browser inside the harness's private network (a digest-pinned Playwright image), through the platform's own origin; its screenshots are written to a directory the harness workflow uploads as an artifact.

## New API (all `Authorization: Bearer`, every statement scoped to the token's user)

| Route | Request | Response |
|---|---|---|
| `GET /v1/me` | — | `{"email"}` |
| `GET /v1/balances` | — | `{"modules":{"m1":{"available","reserved"},"m2":{"available","reserved"}},"m3":{"enabled","until","state"}}` (`state` is `active`, `suspended` or `unknown`) |
| `GET /v1/jobs?status=active` | additive filter (`active` = scheduled, running, paused, cancelling) | as today |
| `POST /v1/templates/check` | `{"subject","html","text"}` | `{"ok":true,"fields":[{"name","has_fallback"}]}` or `{"ok":false,"errors":[{"part","line","column","message"}]}` (status 200 either way) |
| `POST /v1/jobs` (CSV) | query `email_column=<header name>` (default `email`) | as today |
| `GET /v1/suppressions` | `?q=&cursor=&limit=` | `{"entries":[{"email","source","created_at"}],"next_cursor"}` |
| `POST /v1/suppressions` | `{"email"}` | `201 {"entry"}` or `200 {"entry","already":true}`; an invalid address is `400 validation_failed` |
| `POST /v1/suppressions/remove` | `{"email"}` | `{"removed":true\|false}` |
| `POST /v1/suppressions/import` | `text/csv` with an `email` column | `{"added","already_there","invalid","invalid_sample":[{"line","reason"}]}` (reason from a closed vocabulary) |
| `GET /v1/m3/accounts` | `?cursor=` | `{"accounts":[...],"next_cursor"}` (no secret, `has_credential` only, the From-gate state as a word, never the module's text) |
| `POST /v1/m3/accounts` | the adapter's new-account fields plus `acknowledge_from_gate` | `201 {"account"}` |
| `POST /v1/m3/accounts/{id}/test` | `{"acknowledge":bool}` | `{"result":{"ok","stage","from_gate"}}` |
| `POST /v1/m3/accounts/bulk` | JSON list or `text/csv` | `{"results":[{"line","ok","code"}]}` |
| `POST /v1/m3/accounts/{id}/remove` | — | `{"removed":true}` |

Errors keep the one envelope (`{"error":{"code","message","request_id"}}`); every new code is added to `Docs/jobs-api.md`, to the classify table, and to the copy catalogue (the gate A8 holds the three equal).

## Conventions the screens and the end-to-end specs share

- Elements are found by role and accessible name taken from the copy catalogue (`web/src/copy`), never by CSS class. `data-testid` is used only for these: `job-row-<id>`, `job-status-<id>`, `balance-<module>`, `sample-<n>`, `preview-frame`, `results-row-<ordinal>`, `account-row-<id>`, `suppression-row-<n>`.
- Every page has one `h1`; every input has a label; focus is visible; the whole app works by keyboard.
- Statuses: valid, risky, invalid, unknown, sent, bounced, failed each have a colour and a word (colour is never the only signal).

## Not checked (named owner or phase)

Real browsers other than the one Playwright image (owner); the public edge, TLS, DNS and the client address behind the proxy (P8); the admin console and the invite sender (P7: the invite link is minted in the e2e scope by GoTrue's admin route); a real mailbox provider (owner smoke test); the disclosure wordings are placeholders until P8 drafts them with the terms.

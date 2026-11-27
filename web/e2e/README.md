# The browser end-to-end tests

Playwright specs (`*.spec.ts`) that drive the built web app in Chromium against the real stack. They run in the harness scope **`e2e`** (`tests/harness/e2e/stage.sh`, `tests/harness/docker-compose.e2e.yml`, dispatched from the `harness` workflow) and nowhere else. A developer's machine usually has no Docker, so **the GitHub runner is the evidence**; locally you can only type-check (`npm run typecheck`) and list (`npm run e2e:list`, no browser, no stack: the `web` workflow does this on every push to keep the specs loading).

## What the stack is

Everything is on the harness's private network (`internal: true`, no published port). The browser container is inside it.

| Name (inside the network) | What |
|---|---|
| `https://app.harness.test` | the platform: the built app, `/config.json`, `/v1/...`, `/u/<token>` (one origin) behind a TLS sidecar (nginx, a per-run **self-signed** certificate; the config sets `ignoreHTTPSErrors`) |
| `https://auth.harness.test/auth/v1` | a real, digest-pinned GoTrue, at the path a hosted Supabase project serves it at; this is `/config.json`'s `supabase_url` and the issuer of every token |
| `http://smtp-ctl:8025` | the controllable SMTP sink's control API (what modules 2 and 3 deliver to) |
| modules 1, 2, 3 | real, unmodified, with their workers; module 1 asks a **fixture DNS** and its vendor fallback is **on**, wired to module 1's own ZeroBounce stand-in |

The engine runs with a 200 ms tick, so a small job finishes in seconds. Nothing reaches the internet.

## Environment (read lazily by `support/env.ts`; `--list` needs none)

`E2E_BASE_URL`, `E2E_GOTRUE_URL`, `E2E_ANON_KEY` (public by design), `E2E_SINK_URL`, `E2E_M3_SINK_HOST`, `E2E_FIXTURES`, `E2E_ARTIFACTS_DIR`, `E2E_SHOTS_DIR`. No service-role key, JWT secret or database credential is in the container: a spec cannot make users or write the database (the stage did, before the run).

## The users (`support/users.ts`, from the fixtures file the stage wrote)

| tag | funded | use it for |
|---|---|---|
| `a` | 100000 credits at module 1 and at module 2, module 3 enabled, a sending account at module 2 | the main user of most specs |
| `b` | the same | the **other** user of the cross-user specs (A must never see B's jobs, accounts, suppressions or results, in the UI and in the API) |
| `c` | the same | the one spec that opens an **invite link in the browser** and sets a password on the screen. Nothing in the helpers ever opens `c`'s invite |
| `s` | nothing: no credits, no entitlement, no sending account | the error states (a low balance, module 3 not enabled, no sending account) |

Every user has an `invite_link` (minted by GoTrue's admin route; open it with `page.goto(link)`: GoTrue verifies it and sends the browser to the app with the session in the URL fragment, which is what the invite screen consumes) and a per-run `password`. `ready(request, 'a')` gives a signed-in session **without a screen** (it signs in; the first time it opens the invite WITHOUT a browser and sets the password with the invite's own tokens); it keeps no state, so call it at the top of any test. `bearer(session)` is the header of an API call. For a spec that signs in **through the screen**, use the user's `email` and `password` (call `ready` first for `a`, `b` and `s`, so the password exists).

**The users are shared by the whole run** (one worker, files in order): their balances, jobs, suppression lists and module 3 accounts accumulate across specs. Do not assume an empty state; make your own data (unique recipients, below) and assert what changed. Specs must not depend on the order of the files.

## Data

`support/data.ts`: `recipients(tag, n)` makes `<tag>-01@example.test`... (**unique to a spec**, so another spec's unsubscribe cannot suppress them and the sink can be read for them alone), `sendCsv`, `csv`, `csvFile`, `fixturePath`. The files of `fixtures/`:

| file | what the stack makes of it (derived from the harness's own proofs; the first run confirms each) |
|---|---|
| `verify-list.csv` (12 lines: `email,first_name,company`) | 1 malformed line (`not-an-address`), 1 repeat (`ANA@...` of `ana@...`), 10 to check. `*@free.example.test` (4): has an MX, so **risky / plausible**, free. `*@d1..d4.example.test` (4): no MX in the fixture DNS, so **invalid / no_mx**, charged. `valid+e2e0000N@provider.example.test` (2): routed to the vendor stand-in, which answers valid: **risky / externally verified**, charged at the vendor's price. The vendor is ON, so the preview of a customer who has not opted out is **2 credits per row** and 1 per row for one who has (settings: vendor opt-out) |
| `send-list.csv` (`email,first_name,topic`) | six recipients the sink accepts, except `bounce@example.test` once the spec sets `sinkMode(request, { reject_rcpt: ['bounce@example.test'] })` (a 550 at RCPT TO: a bounce). A blank `first_name` or `topic` takes the template's fallback |
| `suppression-import.csv` (`email`) | 3 addresses, 1 repeat, 1 invalid: the import's `added`, `already_there` and `invalid` |

A send job's recipients may be any address: the senders relay through the user's account to the sink, nothing resolves the recipient's domain.

## Sending: modules 2 and 3 and the sink (`support/sink.ts`)

* Module 2 needs nothing from the screens: users `a`, `b`, `c` already have a dedicated pool and an account that names the sink.
* **Module 3 account through the screens** (A7/A10): host `E2E_M3_SINK_HOST` (`10.smtp-ctl.example.test`, a private-looking alias of the sink: the account verifies), port `1025`, any username and secret (the sink accepts any credential), and a From address **`...@example.test`**: the From-gate runs in **enforce** against a fixture DNS where `example.test` is aligned (the screen shows the aligned result). A From domain outside the fixture (for instance `x@unaligned.test`) is **not** aligned: use it for the other From-gate result. Module 3 sends on one lane with at most a second between two sends.
* **Bounces, deferrals and slowness** are the sink's mode, per recipient: `sinkMode(request, { reject_rcpt: [...] })`, `defer_rcpt`, and `data_delay_ms` with `delay_recipients` (a message to those recipients stays in flight that long: the window a pause or a cancel needs; 2 to 3 seconds each). The mode REPLACES the previous one: put the default back with `sinkMode(request, {})` when you are done. **Never reset the sink** (`POST /api/reset`): other specs' messages are in it.
* **The unsubscribe link of a message**: `const [m] = await waitForMessages(request, address)`, `unsubscribeLink(await rawMessage(request, m.seq))`, then `page.goto(link)` in the BROWSER (it is under `https://app.harness.test/u/<token>`). The next job of the same user excludes that address (suppressed).
* The sender's `Message-ID`, `To`, `Subject` and bodies are in the raw message; `headerOf(raw, 'Subject')` unfolds a header.

## The three helpers every screen spec uses

* `shot(page, 'job-detail-running')` (`support/shots.ts`): a full-page PNG under `E2E_SHOTS_DIR`, uploaded with the run. One name per main screen and state, lower case, digits and hyphens. The stage fails a run with fewer pictures than `E2E_MIN_SHOTS`.
* `expectNoA11yViolations(page)` (`support/axe.ts`): axe-core, WCAG 2 A and AA, contrast included, **fails on any violation**. Call it once the screen has settled (data in, spinner gone). `exclude: ['[data-testid=preview-frame]']` leaves the sandboxed preview of a user's own HTML out; never exclude the page.
* `expectNoInternalWords(page)` (`support/copy.ts`): the readable text of the page (and the attributes a screen reader speaks) holds none of the words of `web/src/copy/internal-words.json`. **That file is a STUB until the lead replaces it** (an array of strings, or an object with `words`).

## Rules for a spec

* Find elements by **role and accessible name taken from the copy catalogue** (`web/src/copy`), never by CSS class. `data-testid` only for the ones the spec lists (`job-row-<id>`, `job-status-<id>`, `balance-<module>`, `sample-<n>`, `preview-frame`, `results-row-<ordinal>`, `account-row-<id>`, `suppression-row-<n>`).
* **No `test.skip`, `test.fixme`, `test.only` or `test.fail`**: a test that cannot run FAILS. The stage refuses a report with a skipped test and `forbidOnly` refuses an `only`. A paired control (a check that must fail on a bad page) belongs next to each check, as in `smoke.spec.ts`.
* **Never put a token, a key, an invite link or a password in an assertion message or a `console.log`**: they are secrets of a throw-away stack and the stage refuses a run whose output holds a secret or anything shaped like a JWT. Compare booleans and statuses (`expect(x === y, 'what it means').toBe(true)`), not values.
* Wait for what you assert (`expect(locator).toHaveText(...)`, `expect.poll(...)`), never for a fixed time; a job moves within seconds, a pause or cancel needs a send in flight (the sink's `data_delay_ms`).
* One worker, no retry: a flaky test is a failed test.

## What the stage checks around the run (`tests/harness/e2e/stage.sh`)

The specs are listed first (`E2E_MIN_TESTS`, **raise it as specs land**: it is the scope's own floor and it is not in `tests/floors-harness.txt`), then run; the stage reads `results.json` (`check-report.mjs`: nothing failed, nothing skipped, nothing flaky, as many ran as were listed), at least `E2E_MIN_SHOTS` pictures exist, and every file of the artifacts directory (the zipped traces unpacked) is read for the run's infrastructure secrets (the directory is emptied on a hit). The services' logs and the run's own output are read for secrets and JWT-shaped strings. The traces of a FAILED test hold the test users' passwords and tokens by nature (Playwright records requests and typed values): they belong to a stack that is gone when the run ends.

## Not covered by this scope

Chromium only; a self-signed certificate and no real edge; no mail leaves GoTrue (the invite is minted by the admin route and the recovery mail is never sent: a "forgot password" screen shows its confirmation, the mail is not followed); a fixture DNS for module 1 and for the From-gate; the sink, never a real mailbox provider; one worker.

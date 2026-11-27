# web: the customer app

React 19, TypeScript, Tailwind 4, Vite, Vitest. No UI kit, no router, no state library, no fetch wrapper from outside: all of it is in `src/` (decision 3 of
`Docs/phase-6-spec.md`). The platform serves the built app and the API from one origin, under a strict CSP (`script-src 'self'`).

```
npm run typecheck && npm run build && npm run check:css && npx vitest run     # what CI runs
node scripts/check-csp.mjs && node scripts/check-csp.mjs --selftest           # the built page holds no inline script (see "Build hygiene")
```

## The map

| Path | What |
|---|---|
| `src/design/tokens.css` | THE tokens: colour (light, dark, status), type, spacing, radius, shadow. A page never writes a colour. `tokens.test.ts` computes WCAG contrast from it |
| `src/ui/` | the components (import from `src/ui/index.ts`) |
| `src/router.tsx`, `src/routes.ts` | the router and the route registry |
| `src/pages/<area>/` | one folder per area; `routes.ts` in each is **the only file that registers a page** |
| `src/copy/` | the catalogue: `common.ts`, `auth.ts`, `errors.ts` (mine) and one file per area (the area's agent) |
| `src/api/` | `client.ts` (`api.get/post/put/del`), `endpoints.ts` (one typed function per route), `types.ts`, `errors.ts`, `poll.ts` |
| `src/auth/` | the session (in memory), the sign-in client (`gotrue.ts`), the guard |
| `src/shell/` | the frames (top bar, navigation, skip link), theme, error boundary, offline banner |
| `src/test/helpers.tsx` | `renderApp(path)`, `signInForTest()`, `mockApi({...})`, `resetForTest()`: for your page tests |

## Building a screen (areas: dashboard, jobs, newjob, accounts, suppressions, settings)

You touch **only** `src/pages/<your area>/` and `src/copy/<your area>.ts` (and your tests). Never `routes.ts` at the top, never `common.ts`, `errors.ts`, `ui/`.

1. **Register the page** in `src/pages/<area>/routes.ts` (`export const routes: RouteDef[] = []` is waiting for you):
   `{ path: '/jobs/:id', title: 'jobs.detail.title', element: JobDetailPage, protected: true }`. `title` is a copy key (a missing key does not compile).
   `element` is a component with no props (or `React.lazy`); read the URL with `useRoute().params`, `useSearchParams()`. **Every page of an area is `protected: true`**
   (a test fails otherwise); a protected route renders nothing, not even a flash, until the user is signed in. The navigation links to `/`, `/jobs`,
   `/new/verify`, `/new/send`, `/accounts`, `/suppressions`, `/settings`: register a page at the ones of your area (a test checks it as soon as your area has one page).
2. **Render the page's `<PageHeader title={...} />` at once**, before any data has loaded: it is the page's one `<h1>`, and the router focuses it on arrival
   (and scrolls to the top). A page with no `h1` is a page whose focus falls back to `<main>`.
3. **Write every sentence in `src/copy/<area>.ts`** under keys `<area>.…` and read it with `t('jobs.title', { count })`. `{name}` is filled from the second argument;
   for a plural write two keys. Rules `copy.test.ts` holds: no empty string, no snake_case token, no backtick, no internal word (`module`, `engine`, `slug`, `preview_stale`, the
   words of `src/copy/internal-words.json`), no API code. A key used in `src/**` that no copy file defines is a red test.
4. **Words that come from the API are not copy to invent**: `jobStatusText`, `resultStateText`, `resultReasonText`, `malformedReasonText`, `moduleText`, `addressReasonOf`,
   `fieldIssuesOf` (`src/copy/words.ts`) turn a status, a state, a reason into a sentence, and an unknown word into a generic one, never the word.
   `<StatusBadge status={jobStatusToken(job.status)} label={jobStatusText(job.status)} />` (`src/ui/status.ts`) colours the same state the same way everywhere.
5. **Call the API through `src/api/endpoints.ts`** (`jobsApi.list`, `suppressionsApi.add`, ...). Show a failure with `errorText(err)` (and `errorReference(err)` for the support
   reference); never `err.message`, never `err.code`. A template mistake is `templateProblem(err)`. Poll with `usePolling(fn, ms, { enabled })` (stops with the tab and the page).
   A confirm needs `newIdempotencyKey()` made **once per intent** and sent again on a retry. The CSV upload takes the `File` itself: `jobsApi.createCsv(file, { module, email_column })`.
   A download is `saveBlob(await jobsApi.resultsCsv(id), 'job.csv')`.
6. **Compose from `src/ui/`** and the tokens (`bg-surface`, `bg-raised`, `text-fg`, `text-fg-muted`, `border-border`, `bg-accent`, `text-status-valid` ...). The default Tailwind palette
   is removed on purpose. Wrap every input in `FormField`; every icon button has a required `label`; a row action opens a `ConfirmDialog` when it cannot be undone.
7. **Test it** with `renderApp('/jobs')` after `await signInForTest()` and `mockApi({ 'GET /v1/jobs': { jobs: [], next_cursor: null } })`. Find elements by role and
   accessible name (the words come from the catalogue: `t('jobs.title')`). `data-testid` only for the ones the spec lists (`job-row-<id>`, `job-status-<id>`,
   `balance-<module>`, `sample-<n>`, `preview-frame`, `results-row-<ordinal>`, `account-row-<id>`, `suppression-row-<n>`). Every test has a paired control.

## Contracts worth knowing

- **`src/api/types.ts` is the contract to reconcile.** Part 1 is verified against the Go views; part 2 (me, balances, template check, suppressions, module 3 accounts) follows the
  new sections of `Docs/jobs-api.md` as they were on 2026-11-28 while those routes were still being built. If an answer differs, fix `types.ts`, `endpoints.ts` and `copy/errors.ts` together.
  `copy.test.ts` reads the error table of `Docs/jobs-api.md` and is red until every code and every `not_allowed` reason in it has a customer line in `src/copy/errors.ts`.
- **The session lives in memory only** (`src/auth/session.ts`). No storage API is called anywhere except `src/shell/theme.ts` (the word "light" or "dark"): a scan fails otherwise.
  A reload signs the user out; an expired token is refreshed silently a minute before `exp` and once more after a 401; a refused refresh ends the session and the router guard
  sends the user to the sign-in page (which says why). A page restored from the back-forward cache is wiped.
- **An invite or reset link** lands with `#access_token=…&type=invite|recovery`: `main.tsx` reads it once, removes it from the address bar and keeps the tokens in memory
  (`auth/hash.ts`); the app then shows only the choose-a-password page. Password rule: 12 to 72 characters (`auth/password.ts`), stated on the page.
- **The email links** (invite, reset) point where GoTrue's Site URL points: that must be the platform's public origin (the app sends no `redirect_to`), and the link carries the
  session in the URL hash, which the app reads once.
- **`/config.json`**: `supabase_url` is the address GoTrue answers at. A hosted project given as its bare origin (`https://<ref>.supabase.co`, what `install.md` says) gets `/auth/v1`
  added; any other URL is used as it is (the e2e GoTrue answers at the root).
- **Light and dark**: the system preference decides until the user picks one with the toggle (`data-theme` on `<html>`, one stored word). A stored override that differs
  from the system can flash once before the first paint (no inline script is allowed to prevent it).

## Build hygiene (CSP)

`index.html` has no inline script and no inline handler; `vite.config.ts` sets `build.modulePreload.polyfill = false` and **fails the build** if the built page holds an inline
`<script>` body, a script without a `src`, an `on*=` attribute, a `javascript:` URL or anything from another origin (the same function as `scripts/check-csp.mjs`).
`check:css` still looks for `min-h-screen` (the class of the app's root element in `App.tsx`); do not remove it.

To run the CSP check in CI (needs `package.json` and `.github/workflows/web.yml`, which another agent owns): add the script `"check:csp": "node scripts/check-csp.mjs"` and two steps after
`check:css`: `npm run check:csp` and `node scripts/check-csp.mjs --selftest`.

## Decisions the spec did not name

Hand-written focus trap (not `<dialog>`); the router renders nothing while a guard redirects; `404`s of `/v1/me` are tolerated (the top bar shows no name); a refused refresh signs out,
a refresh that could not be made (offline) does not; `busy` is retried twice after `Retry-After` (never more than 5 s), no other 503 is; the forgot-password page says the same thing
whatever GoTrue answered about the address; sign out asks GoTrue to end this session only (`scope=local`); job and result states get their colours from `ui/status.ts`, their words from `copy/common.ts`.

## Not checked here

A real browser (the `e2e` scope), the real CSP header of the Go service, a real GoTrue (the e2e scope mints the invite), the look of the pages (no screenshot is compared), screen reader speech
(roles, names and descriptions are asserted, not heard), `prefers-color-scheme` (jsdom has no media query: contrast is computed from the token file instead).

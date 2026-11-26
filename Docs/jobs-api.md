# The job API

The HTTP API a signed-in user creates, previews, confirms, watches and downloads a job through (Phase 4, chunk B14b; `internal/api`). It is the only surface the web app (Phase 6) is built on, and it is
small on purpose: **every rule of a job is the job store's and the database's** (`internal/jobs`, `platform.jobs_transition_guard`, the slot index, the reserve), so the API translates a request into a
store call and the store's answer into JSON, and a rule never lives in a handler.

A service started with **no key source** (neither `PLATFORM_JWT_SECRET` nor `PLATFORM_JWKS_URL`, see [`install.md`](install.md)) does not mount these routes: a request for one gets the `404 not_found`
envelope of any unknown path, and the start-up log says `job API authentication is not configured`. With one, the log says `job API mounted`.

## Authentication

Every route needs `Authorization: Bearer <token>`, where the token is the **access token Supabase gave the signed-in user** and nothing else.

- **Exactly one `Authorization` header.** The scheme is `Bearer` in any letter case; the token is at most **8 KiB**. A token in the query string, a cookie or another header is not read.
- The token is verified (signature, `exp`, `nbf`, `iss`, `aud`: [`config-reference.md`](config-reference.md), "How the job API verifies a login") and its `sub` is resolved to a platform user. An
  unknown login, an inactive user and the system user are refused.
- **Every one of those failures is the same answer, except one (`503 auth_unavailable`, below)**: `401`, the code `unauthenticated`, the same message, `WWW-Authenticate: Bearer`. Nothing in the response says why (an expired token and a forged one are
  indistinguishable); the reason is in the service's log, under the request id, as a word, never the token or a part of it.
- **The one exception is the platform's own inability to judge the token**: when the signing keys cannot be obtained (the JWKS endpoint is down, times out, answers garbage, or the key set was never loaded) the answer is `503 auth_unavailable` with `Retry-After: 30`, never a 401 that blames the user. A token that is bad by itself (expired, wrong audience or issuer, not yet valid) stays a 401 even then; an HS256 token is judged by its secret alone and never meets the 503; an unknown key id within 30 seconds of a good fetch of the key set is a 401 (retry).
- A failure of the platform's own database while the login is resolved is **not** a refusal of the login (the token verified): it is `500 internal_error`, so that a client does not sign a user out for an outage.
- **The user is the principal and nothing else.** The user a request acts for is the platform user of the verified login. No path, query, header or body names a user: a body or a query that tries (`user_id`, `owner`, ...)
  is refused as an unknown member (`400 bad_request`) and reaches nothing. A job of another user is `404 not_found`, **the same answer, byte for byte but for the request id, as a job that does not exist**.

## Conventions

- JSON in, JSON out, `Content-Type: application/json; charset=utf-8` (a create may also send `text/csv`; a download answers `text/csv`). Members the API does not know are refused (`400 bad_request`), at any depth.
- **Ids** are lower-case uuids, **times** are RFC 3339 in UTC (`2026-12-01T09:30:00Z`, with nanoseconds only when there are some). A time or an id that does not apply is `null`. The job never shows its owner (`user_id`) or the
  confirm key, and no response shows an internal constraint name or SQL.
- Every response has an `X-Request-Id` (`req_` and 32 hex digits), the same id as in the error body and in the service's log lines for the request. Authenticated responses are `Cache-Control: no-store`.
- A request with a body larger than its cap is `413 payload_too_large` (the declared `Content-Length` is judged before a byte is read): **64 MiB** for `POST /v1/jobs`, **64 KiB** for every other route (a route that takes no body
  takes none beyond 64 KiB either).
- **Query parameters** other than the ones a route lists are refused (`400 bad_request`), as is a parameter given twice. An empty value is the same as an absent one. A `limit` or `after` that is not a whole number in its
  range is `400 validation_failed`.
- A wrong verb on a route is `405 method_not_allowed` with an `Allow` header, an unknown path `404 not_found`: the platform's own envelope, before authentication.

## The routes

| Route | What it does | Success |
|---|---|---|
| `POST /v1/jobs` | create a draft from a list (JSON or CSV) | `201`, the job, `Location: /v1/jobs/{id}` |
| `GET /v1/jobs` | the user's jobs, newest first (`cursor`, `limit` 1 to 100, default 25) | `200`, `{"jobs":[...],"next_cursor":...}` |
| `GET /v1/jobs/{id}` | one job | `200`, `{"job":{...}}` |
| `POST /v1/jobs/{id}/preview` | compute what the list will cost; a draft becomes `previewed`; optional body `{"unknown_choice"?}` | `200`, the job with its `preview` |
| `POST /v1/jobs/{id}/confirm` | confirm a previewed job: `Idempotency-Key` required; body `{"fingerprint","scheduled_start"?,"unknown_choice"?}` | `200`, the job and what the confirm did |
| `POST /v1/jobs/{id}/discard` | cancel a draft or a previewed job (it holds nothing) | `200`, the job |
| `POST /v1/jobs/{id}/reopen` | take a previewed job back to draft (its preview is cleared) | `200`, the job |
| `POST /v1/jobs/{id}/cancel` | cancel before the job started (credits come back), or ask for a cancel of a running or paused-while-running job (it becomes `cancelling` and ends `cancelled`) | `200`, the job |
| `POST /v1/jobs/{id}/pause` | defer a **scheduled** job, or hold a **running** job of module 2 or 3, until it is resumed | `200`, the job |
| `POST /v1/jobs/{id}/resume` | put a paused job back to the state it paused from (`scheduled` or `running`) | `200`, the job |
| `GET /v1/jobs/{id}/events` | the job's history (`after`, `limit` 1 to 1000, default 100) | `200`, `{"events":[...],"next_after":...}` |
| `GET /v1/jobs/{id}/results` | the results by row (`cursor`, `limit`) | `200`, `{"results":[...],"next_cursor":...}` |
| `GET /v1/jobs/{id}/results.csv` | every result as a CSV, streamed | `200`, `text/csv` |

The five moves (`discard`, `reopen`, `cancel`, `pause`, `resume`) take no body. **What a module can do to a running job is data** (`platform.modules.can_pause`, `can_cancel`), and the database refuses what it cannot:
modules 2 and 3 can pause and resume one running job, every module can cancel one, and module 1 has no pause. The API does not pretend: each move attempts the right move for the job's state and the database
decides. A `pause` of a running module 1 job is `409 not_allowed` with `details.reason` `pause_not_supported`. The word `cancel_not_supported` is **reserved**: no module the platform seeds is refused a cancel
any more, so it is returned only if a module's capability row says it cannot cancel.

### Pause, resume and cancel of a running job

| Request | The job is | The answer | What happens next |
|---|---|---|---|
| `pause`, module 2 or 3 | `running` | `200`, `status: paused`, `paused_from: running` | the engine tells the module at its next tick; the items the module had already claimed finish and are reported once |
| `pause`, module 1 | `running` | `409 not_allowed`, `reason: pause_not_supported` | nothing changed |
| `resume` | `paused` from `running` | `200`, `status: running` | the engine tells the module to let the campaign go on; the rest of the list is submitted and the job completes |
| `resume` | `paused` from `scheduled` | `200`, `status: scheduled` (`start_deadline_passed` when its deadline has passed) | the engine starts it at its time |
| `cancel`, any module | `running`, or `paused` from `running` | `200`, `status: cancelling` | the engine tells the module, records what finished, releases the rest of the reserve once and ends the job `cancelled` |
| `cancel` | `confirmed`, `scheduled`, or `paused` from `scheduled` | `200`, `status: cancelled` | nothing was submitted: every row has the platform's free `cancelled` result and the whole reserve is back |

**The lag is real and it is visible.** A job is `paused` the moment the API answers, but the module is told by the engine at its next tick (at most one tick, 1.5 seconds by default, once the engine looks at
the job), and a module that claimed an item a moment before keeps working that item: it finishes and is reported once, settled and charged as any item is. When the module's own report first says the campaign
is held, the engine writes a `note` event whose `reason` begins `module_paused`; when it stops saying so, one that begins `module_resumed`. The job's `paused` transition event is the API's answer, the `note` is
the module's acknowledgement, and the time between them is the lag. A module that does not acknowledge keeps being asked at every look.

**A paused job holds its credits.** The reserve and the one-active-job slot stay held while the job is paused (and while it is `cancelling`) until the user resumes it or cancels it. **There is no automatic expiry
of a pause taken while the job was running** (a job that was running has no start to miss, and the database refuses to expire it); a job paused *before* its start still expires at its start deadline. While a
job is paused the engine submits nothing and raises no `stale` alert (nothing is expected to progress); when the job resumes its staleness clock starts again. If everything the module held finishes while the
job is paused, the job completes the normal way.

**Module 1's cancel** is real: the rows it had not started become `cancelled` results (free: `cancelled_by_caller`, never a verdict), the rows it was already checking finish and are charged as the charge table
says, and the module's own counts add up as `submitted = settled + cancelled + pending` (the platform's results hold the cancelled rows as `cancelled`, decision `free`, rule `M1-24`). A cancel of a job that
had nothing left to cancel still ends `cancelled`: the request was accepted.

### The job

```json
{
  "job": {
    "id": "0b000000-0000-4000-8000-000000000001",
    "module": "m1",
    "status": "previewed",
    "source_job_id": null,
    "scheduled_start": null,
    "start_deadline": null,
    "paused_from": null,
    "settings": {"unknown_choice": null},
    "preview": {
      "rows": 5, "pending": 3, "malformed": 1, "malformed_reasons": {"no_at": 1}, "duplicates": 1, "excluded": 0, "suppressed": 1, "suppressed_still_verified": true, "missing_fields": 0,
      "units": 3, "selected": 0, "unknown_choice": null, "unknown_count": 0, "unknown_choice_required": false,
      "metered": true, "rate_unit": "m1_own", "unit_cost": 1, "credits": 3, "available": 100, "shortfall": 0,
      "vendor_reachable_known": true,
      "fingerprint": "3f9a...c1", "computed_at": "2026-12-01T09:00:00.123456Z"
    },
    "preview_fingerprint": "3f9a...c1",
    "previewed_at": "2026-12-01T09:00:00.123456Z",
    "confirmed_at": null, "started_at": null, "finished_at": null, "cancel_requested_at": null,
    "created_at": "2026-12-01T08:59:00Z", "updated_at": "2026-12-01T09:00:00Z"
  }
}
```

`status` is one of `draft`, `previewed`, `confirmed`, `scheduled`, `running`, `paused`, `cancelling`, `completing`, `succeeded`, `cancelled`, `expired`, `failed`. A send job (modules 2 and 3) also carries
`content` (`subject`, `html`, `text`, `reply_to`), which the list leaves out. The `preview` counts are the store's: `credits` is what a confirm would hold, `shortfall` what the balance lacks of it, and the
`fingerprint` names everything the numbers depended on (a confirm refuses a stale one).

**Suppression applies to send jobs only** (modules 2 and 3). `suppressed` is how many pending rows the user's own suppression list holds. In a **send** job they are left out at the confirm (never sent, never
reserved, a free `suppressed` result) and `units` and `credits` do not count them. In a **verify** job (module 1) nobody is e-mailed, so they are **not** skipped: they are verified and charged like any other
row, `units` and `credits` count them, `suppressed_still_verified` is `true`, and the results carry `"suppressed": true` on them (below). A chained send still excludes them: its rows are the verify job's results,
and the user's list is applied again at the send's own confirm, so an address suppressed after the verify job finished is not sent to.

**`vendor_reachable_known`** (a module 1 job): module 1 says in its `describe` whether its vendor fallback is reachable NOW (fallback enabled, DPA confirmed, key present, spend cap above zero, kill switch off), and the platform reads that at every preview and every confirm, so the reserve (1 credit per row with the vendor off, 2 with it on) follows module 1's own configuration and there is no platform dial for it. `vendor_reachable_known` is `true` when module 1 answered for this preview, `false` when it could not be read (a stored reading is then used) and for jobs of any other module. A confirm made under the other reachability than the one the preview saw is `409 preview_stale`; an IMMEDIATE confirm that cannot be priced (module 1 unreadable and the stored reading was never set by module 1) is `503 module_unavailable`; a scheduled confirm is not refused for it. A job already confirmed never changes its reserve or its price snapshot.

**The `unknown` choice of a chained send is never defaulted.** `settings.unknown_choice` and `preview.unknown_choice` are `null` until the user says `include` or `exclude`; until then the `unknown` rows are shown
as excluded (the safe reading) and `selected` counts them out. `preview.unknown_count` is how many of the source job's results are `unknown`, and `preview.unknown_choice_required` is `true` when there is at least
one: the confirm then **must** carry `unknown_choice`, equal to the one the preview shows. A job that is not chained has none of this (`unknown_count` 0, `required` false, and a choice on it is `400 validation_failed`).

### Create a draft

`POST /v1/jobs` with `Content-Type: application/json`:

```json
{
  "module": "m2",
  "rows": [
    {"email": "ann@example.test", "fields": {"first_name": "Ann"}},
    {"email": "bob@example.test"}
  ],
  "content": {"subject": "Hello", "html": "<p>Hi</p>", "text": "Hi", "reply_to": "me@example.test"}
}
```

(`source_job_id` and `settings.unknown_choice` are for a chained send only: a choice on a list that is not chained is `400 validation_failed`, `exclude` as much as `include`.)

`module` is `m1` (verify, no content), `m2` or `m3` (send, content required). A send **chained** to a succeeded verify job of the same user names it in `source_job_id` and sends no rows: its rows are that job's results,
selected by verdict (`valid` and `risky` in, `invalid` out, `unknown` by `settings.unknown_choice`: `include` or `exclude`, **no default**; it may be left out here and given at the preview, see below).
`fields` values are strings.

**Or a CSV body** (`Content-Type: text/csv`): a header row is required, the column named `email` (any letter case, surrounding space ignored) is the address and **every other column becomes a merge field** named by its
header. The rest travels in the query, percent-encoded: `module`, `source_job_id`, `unknown_choice`, `subject`, `html`, `text`, `reply_to` (**64 KiB of query in all**; any other parameter is refused).

```
POST /v1/jobs?module=m2&subject=Hello&html=%3Cp%3EHi%3C%2Fp%3E
Content-Type: text/csv

email,first_name
ann@example.test,Ann
bob@example.test,Bob
```

A UTF-8 byte order mark before the header is ignored; a row with another number of columns than the header, a header with no `email` column, an empty or repeated column name, or a bad quote is `400 validation_failed`
naming the line and never quoting the file. A CSV with no rows is a CSV with no list (`validation_failed`), except for a chained send, whose body is empty. An exported `results.csv` has one apostrophe in front of some cells
(below): strip it before importing one.

**The row cap is the store's** (`PLATFORM_JOB_MAX_ROWS`, default 100000): a longer list is `413 too_many_rows` with `details.limit`; a CSV is cut off at the limit while it is read, so a list far over it is refused without
being read to its end.

### Preview, and the `unknown` choice

`POST /v1/jobs/{id}/preview` takes **no body, or an optional JSON body** `{"unknown_choice": "include"}` (or `"exclude"`; at most 64 KiB; a body that is not JSON is `415`, a member the API does not know is
`400 bad_request`). The choice is for a **chained send** only: it is recorded in the job's settings and the source job's `unknown` rows are reclassified (pending for `include`, excluded for `exclude`) while the job
is draft or previewed, so the answer is the preview of the list *under that choice*, with the fingerprint that names it. On a job that is not chained, or with a value that is neither, it is
`400 validation_failed` and nothing is written. `POST /v1/jobs` may also carry the choice (`settings.unknown_choice`, or the `unknown_choice` query of a CSV create) with the same rules.

### Confirm, and the idempotency rule

```
POST /v1/jobs/{id}/confirm
Idempotency-Key: send-001
Content-Type: application/json

{"fingerprint": "3f9a...c1", "scheduled_start": "2026-12-02T09:30:00Z", "unknown_choice": "include"}
```

`Idempotency-Key` is **required, exactly once** (1 to 128 characters of letters, digits and `. _ : -`): a missing one is `400 validation_failed`. `fingerprint` is the one of the preview the user saw; `scheduled_start`
is optional (between one minute and 366 days ahead by the database's clock: the job is then `scheduled` and waits for its time, holding the slot and the credits). `unknown_choice` is **required when the
chained send's source job holds at least one `unknown` result** (`preview.unknown_choice_required`): with none the answer is `422 unknown_choice_required` (`details.unknown_rows` and `details.allowed`) and
**nothing is written** (no credits held, the job is still previewed); with one that is not the choice the job was previewed under it is `409 preview_stale` with `details.reason` `unknown_choice_changed` (preview again
with the choice you want), so nobody confirms a list under a choice they did not see. With no unknown result no choice is needed and one given changes nothing; on a job that is not chained it is `400 validation_failed`.

- **The same key on the same job is one confirm.** The first request does the work; a second (a retry, a double click, a lost response) is answered `200` with `"replayed": true` and the numbers the first one had, and
  writes nothing. The same key on **another** job of the user is `409 idempotency_key_reused`.
- The answer: `{"job": {...}, "replayed": false, "reserved": 3, "pending": 3, "suppressed": 0}`: `reserved` is the credits held (0 for module 3), `pending` the rows that will reach the module, `suppressed` the rows
  that carry the user's suppression flag: in a **send** job they were left out (and are not in `pending`); in a **verify** job they are still in `pending`, verified and reserved, and flagged in the results.
- A confirm is one transaction: it either holds the credits and takes the slot (one active job per user and module) or leaves nothing behind.

### Events

`GET /v1/jobs/{id}/events?after=<id>&limit=<n>` returns the job's history, oldest first: `{"id", "kind", "from_status", "to_status", "actor_kind", "reason", "created_at"}`. `kind` is `created`, `transition`, `stale`,
`flag` or `note`; `actor_kind` is `user`, `admin` or `engine`. `next_after` is the id to pass as `after` for the next page when this one was full, else `null`. An administrator's id and the engine's own notes are not shown.

### Results

`GET /v1/jobs/{id}/results?cursor=&limit=` returns the results of the rows whose outcome is final (a row not final yet has no result), ordered by the row's position (`ordinal`, from 1):

```json
{"results": [
  {"ordinal": 1, "email": "ann@example.test", "origin": "module", "state": "valid", "reason": null, "decision": "charge", "unit": "m1_own", "rule": "M1-03", "recorded_at": "2026-12-01T09:05:00Z", "suppressed": true},
  {"ordinal": 2, "email": "no-at-sign", "origin": "platform", "state": "malformed", "reason": "no_at", "decision": "free", "unit": null, "rule": "P-08", "recorded_at": "2026-12-01T09:05:00Z", "suppressed": false}
], "next_cursor": "bzEw"}
```

`origin` says who decided (`module` or `platform`: the platform's own outcomes are `malformed`, `duplicate`, `suppressed`, `excluded`, `cancelled`, `expired`, `failed`), `decision` is `charge`, `free` or `unmapped`,
and a charged row names its `unit`. **`suppressed`** (added after the others, which keep their names, meaning and order) is `true` when the address was on the user's suppression list when the job was confirmed.
In a **verify** job that row is a module result like any other (verified, charged) and the flag is how the user learns the address is suppressed; in a **send** job it is the platform's own `suppressed` result (never
sent, free, rule `P-SUP`). It is `false` for every other row. The results are **immutable and keyed by ordinal**, so walking the pages of a **finished** job with `next_cursor` neither repeats nor skips a row; the last page has
`next_cursor: null` (a page is never followed by an empty one). **While a job is running** its results appear in the order the module settles the rows, not the order of the list, so a result can be recorded with an
ordinal a walk has already passed: such a walk shows what was final when it went by, and a client that wants the whole list walks again once the job has `succeeded`. A cursor is opaque: one the API did not give is
`400 validation_failed`. `limit` is 1 to `PLATFORM_RESULTS_PAGE_MAX` (default 1000); without one a page is 100 results, or the dial when it is smaller.

### The CSV download

`GET /v1/jobs/{id}/results.csv` streams **every** result:

```
Content-Type: text/csv; charset=utf-8
Content-Disposition: attachment; filename="job-<id>.csv"

ordinal,email,origin,state,reason,decision,unit,rule,suppressed
1,ann@example.test,module,valid,,charge,m1_own,M1-03,true
2,no-at-sign,platform,malformed,no_at,free,,P-08,false
```

- **One line per result, including the platform's own** (malformed, duplicate, suppressed, excluded), written through `encoding/csv` (commas, quotes and line breaks are quoted by the standard rules), LF line ends.
- **The escape rule** (`internal/csvsafe`): a cell that starts with `=`, `+`, `-`, `@`, TAB, CR or `'` gets **one leading apostrophe**, so that no cell opens as a formula in a spreadsheet (`=cmd|' /C calc'!A0` is written
  `'=cmd|' /C calc'!A0`). The inverse is exact: **strip one leading apostrophe when there is one**. A cell that already started with an apostrophe is prefixed too, so the mapping is one-to-one and a program gets the original back
  (`csvsafe.Unescape`). Not checked: look-alike characters of the triggers (full-width `=` and the like), a trigger after leading white space or a byte order mark, and how a given spreadsheet shows the apostrophe (Excel shows it,
  others hide it).
- **Streamed, a page at a time**: the results are read 1000 at a time and the response is flushed after each page, so the server holds one page whatever the size of the list and a client sees its first rows at once. It stops
  when the client goes away.
- **A failure after the first byte can only abort the connection.** The status line is already out and cannot be taken back, so the server ends the connection without the closing chunk: the client sees a body that ends in an
  error, **never one that looks complete**. A client must treat a download that ends in an error as incomplete and fetch it again; a failure before the first byte (a job that is not the user's, a database down) is the ordinary
  error envelope.

## Errors

The envelope is `{"error": {"code", "message", "request_id"}}`, plus `details` **only** where a code has something to say. `message` is a fixed sentence per code; nothing a client sent, no address, no constraint name and
no SQL is ever in it.

| Status | Code | When | `details` |
|---|---|---|---|
| 401 | `unauthenticated` | the login is refused for any reason (see Authentication) | none; `WWW-Authenticate: Bearer` |
| 404 | `not_found` | no such job **for this user** (also another user's job and an id that is not a job id), or an unknown path | none |
| 400 | `validation_failed` | the request names something that cannot be done with it (the module, the content, the key, the fingerprint, a start time, a `limit`, a cursor, a CSV line) | `reason`: a sentence, never an address |
| 400 | `bad_request` | the body or the query cannot be read: not JSON, an unknown member, a parameter the route does not know or given twice, data after the JSON value | `reason` |
| 413 | `payload_too_large` | the body is over its cap, or the query of a CSV create is over 64 KiB | `limit_bytes` |
| 413 | `too_many_rows` | the list is longer than `PLATFORM_JOB_MAX_ROWS` | `limit` |
| 415 | `unsupported_media_type` | the body is not `application/json` (or `text/csv` on a create) | `reason` |
| 422 | `nothing_to_do` | not one row would reach the module (every row malformed, a repeat, excluded or, **for a send job only**, suppressed: a verify job whose rows are all suppressed is verified) | none |
| 422 | `unknown_choice_required` | the confirm of a chained send whose source job holds `unknown` results carries no `unknown_choice`; nothing was written | `unknown_rows`: how many results are unknown; `allowed`: `["include","exclude"]` |
| 409 | `preview_stale` | what the preview depended on changed (the list, a price, the content, the choice, the source job's results), or the confirm's `unknown_choice` is not the one the job was previewed under: preview again | none, or `reason`: `unknown_choice_changed` |
| 409 | `job_slot_taken` | the user already has an active job on this module; nothing was held | none |
| 409 | `idempotency_key_reused` | the key already confirmed another job of the user | none |
| 409 | `not_previewed` | the job is not previewed (or was confirmed with another key) | none |
| 409 | `infeasible` | module 3 says the campaign cannot run (asked BEFORE the confirm's transaction: a user who already has an active module 3 campaign gets `campaign_active` here and not `job_slot_taken`; nothing was held) | `causes`: the module's words |
| 409 | `not_allowed` | the job's lifecycle does not allow the move | `reason`: `pause_not_supported`, `cancel_not_supported`, `not_a_draft`, `already_a_draft`, `not_confirmed`, `not_paused`, `start_deadline_passed`, `submitted_job_ends_through_the_engine`, `job_status_has_no_preview` or `not_allowed` |
| 402 | `insufficient_credits` | the balance does not cover the whole list | `available`, `needed`, `shortfall` |
| 403 | `no_entitlement` | module 3 is not enabled (or has expired) for the user | none |
| 403 | `suspended` | module 3 is observed holding the user suspended | none |
| 503 | `module_unavailable` | module 3 could not be asked whether the campaign is feasible, or module 1 could not be asked whether its vendor fallback is reachable (so the job cannot be priced), and an immediate confirm will not hold credits for a job that cannot start (a scheduled confirm is not refused for it) | none |
| 503 | `busy` | another operation on the same user's credits did not finish within `PLATFORM_MONEY_LOCK_TIMEOUT_MS`; nothing was written, try again (`Retry-After: 1`) | none |
| 503 | `auth_unavailable` | the platform could not obtain the keys to judge the token (the JWKS endpoint is down, times out, answers garbage, or the key set was never loaded); not the user's fault, retry. A bad token stays the 401, an HS256 token is judged by its secret alone, and an unknown key id within 30 s of a good fetch is a 401 | none; `Retry-After: 30`, no `WWW-Authenticate` |
| 500 | `internal_error` | anything else: the cause is in the service's log under the request id, never in the answer | none |
| 405 | `method_not_allowed` | the path exists for other verbs (`Allow` names them) | none |

`not_allowed`'s `reason` is a word of the API's, never the database's constraint name: the two refusals a module's capabilities cause are `pause_not_supported` (module 1 has no per-job pause) and
`cancel_not_supported` (reserved: no seeded module reaches it), a job the module holds that somebody tried to end without the engine is `submitted_job_ends_through_the_engine`, and every other refusal of the
lifecycle that has no word of its own is `not_allowed`.

## Limits and dials

| Limit | Value | Dial |
|---|---|---|
| rows in a job | 100000 | `PLATFORM_JOB_MAX_ROWS` (1 to 100000) |
| a confirmed or scheduled job's wait for the engine, from its start | 30 minutes | `PLATFORM_JOB_MAX_START_DELAY_MIN` (1 to 1440) |
| results in a JSON page | 1000 | `PLATFORM_RESULTS_PAGE_MAX` (1 to 1000) |
| body of `POST /v1/jobs` | 64 MiB | none |
| body of every other route; query of a CSV create | 64 KiB | none |
| token | 8 KiB | none |
| jobs in a list | 100 | none |
| events in a page | 1000 | none |

A body must arrive within two minutes (a create) or fifteen seconds (every other route), and a download's client must take each page within a minute: the server has no read or write timeout of its own, so these bound a
stalled connection per request.

## Not checked, not done

- CI runs the API against fakes of the store (routing, authentication against the real verifier with keys made at run time, every error, the body caps, the CSV round trip and the streaming) and, on a real database, the
  whole stack in one process (`TestJobsAPI_*`: tokens minted per run, the real stores). A real Supabase token and a browser are not exercised until GoTrue joins the harness (chunk B15).
- No CORS headers: the web app is served from the API's own origin or through a proxy that adds them. No rate limiting. No admin routes: the admin console is another writer through the same store, with its own
  authentication.
- Revocation: a signed-out session's token stays valid until it expires; the platform's own check that the user is active, made on every request, is the control.
- A results page cannot be asked for by a position other than the cursor, and a job's results are not searchable.

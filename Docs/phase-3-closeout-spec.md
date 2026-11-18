# Phase 3 close-out: every open decision and every "noticed" item of Phase 3, closed before Phase 4

Previous: [`phase-3-spec.md`](phase-3-spec.md) (every row CLOSED; platform code head `52fe035`, pinned `cc9cdbd` / tag `phase-3b`; M1 `1fa2550`, M2 `bffd424`, M3 `5dde0c0`; parent `6bd8d73`) and [`phase-3-findings.md`](phase-3-findings.md) (the evidence the owner ruled on). Roadmap and locked decisions: [`coldmail-platform-build-plan.md`](coldmail-platform-build-plan.md). Research the work stands on: [`history/p3-research/`](history/p3-research/). False-green classes every gate below must avoid: [`../deferrals-and-false-greens.md`](../deferrals-and-false-greens.md). **Do not start Phase 4.**

**Evidence rule.** Only a GitHub-runner run on the exact head counts; a head is "verified" only when `check-verified-heads` finds the heavy workflows (`harness`, `db-tests`, `supabase`) and the cheap ones (`go`, `gates`, `gitleaks`, `web`, `platform-check`) green on that SHA (a documentation-only head: push a `phase-*` tag; a module head: its main CI workflow plus gitleaks). No assertion is weakened. **Module API changes are additive only** and frozen doors stay frozen. The repositories are public: no real secret, hostname, IP or personal path in any commit; test secrets are generated at run time. **No history rewrites; the only GitHub deletion authorised is row E1.** `zexSites/codmailling-module-3` is never touched. **One commit is pushed to one ref at a time** (a push of one commit to several refs once failed to start the workflows; rule 25 of `CLAUDE.md`).

## How to resume from this file alone

1. The first row of the tables below whose status is not `CLOSED` is where to continue; an `IN PROGRESS` row names the repository, commit and run in its Evidence cell.
2. Order: **Part A (M1) → Part B (M2) → Part C (M3) → Part D (platform) → Part E (housekeeping)**. A module's own CI is green before the platform pins it (`tools/modules.lock`), and the parent is re-pinned at the end (`bash tools/check-modules-lock.sh <parent>` must say OK).
3. One `fault-proofs` dispatch per part, for new or changed gates only (`ci/faults/p3ca/` to `p3cd/`, one phase directory per part; a patch whose target is a module needs the lock to name the commit it applies to).
4. After every chunk: update the row (status, repo@commit, run URL), commit, push. A `CLOSED` row cites its head (`platform@<sha>`, or the module's own `verify@<sha>`) and its runs, or `check-verified-heads` fails.

## Rulings carried into this session (the owner's; nothing here reopens them)

- **OD-6 / OD-8 (Tier 4): a partial Tier 4 for module 1** (describe, an idempotent job creation, totals on the job status) and no quote/run/report.
- **OD-2:** one derived caller key per platform user, with a key version in the derivation so the secret can be rotated.
- **OD-4:** a node becomes `active` only after a green audit row, and restarts re-audit.
- **OD-9:** the platform's pre-validation is `norm/v1` plus module 1's syntax layer, exactly as module 1 implements it; this corrects the earlier ruling "badly formed addresses are free" for the syntax layer only.
- **OD-1:** module 3 gets `POST /v1/cancel`. **OD-3:** a `details[]` code on module 3's run 409. **OD-5:** module 2's 409 is backpressure (phase 4 owns serialisation). **OD-7:** the token dial is done. **OD-10:** one enforced health action observed in the harness.
- Production runs with module 3's From-gate ON (never allow a wrong From): the harness runs module 3 with the gate ON.

---

## Part A — Module 1 (`coldmail-verify`)

| # | Item | Exit check | Status | Evidence |
|---|---|---|---|---|
| A1 | **Partial Tier 4.** `GET /v1/describe` in the shape family of modules 2 and 3; bulk job creation idempotent on a caller-supplied key (a repeat returns the same job, never a second one); totals on the job status (submitted, settled, pending, duplicates, by status); no quote/run/report. The platform's "orphan job possible" handling removed where the idempotent create makes it unnecessary; each of the 13 special cases listed as removed or kept | module tests for every added route/field with paired controls (a repeat with the same key and the same body is the same job, the same key with another body is refused, no key keeps today's behaviour); the new fields are additive (the existing contract fixtures still match); the platform adapter and live harness tests of D5 | OPEN | — |
| A2 | **Node self-promotion (OD-4).** A node becomes `active` only after a green audit row, with `port25_open` taken from that audit; a control-plane restart re-audits and re-promotes and never trusts a stale `active`; the harness's SQL promotion removed; the lease-recovery wait documented with its dial | a fresh node is promoted after a green audit and stays `provisioning` after a red one (paired control); a restart re-promotes; the harness stage runs on the real path | OPEN | — |
| A3 | **Bad input:** a non-UUID job id answers a 400 or 404 with the envelope, never a 500 | a test per route taking a job id, with a paired control (a valid id still works) | OPEN | — |
| A4 | **Caller-key rotation (OD-2 confirmed).** The derivation context carries a key version (`"m1-caller\|v1\|" + user id`), `PLATFORM_M1_KEY_SECRET` can be rotated by provisioning v2 keys beside v1, the rotation steps documented | a v1 key and a v2 key for the same user both work during the overlap and are distinct; the old v1 vector still derives the key already provisioned (no silent change) | OPEN | — |
| A5 | **One syntax rule (OD-9).** The platform's pre-validation is `norm/v1` plus module 1's syntax layer exactly as module 1 implements it; the shared vector file gains syntax cases and runs through both implementations in CI; `invalid_syntax` never reaches module 1 from the platform (M1-10 stays in the table, unreachable from the platform, a test proves it); the suppressions use the same Go validator, and for the 22 vectors where the table's CHECK differs from the validator the CHECK is made equal or proven only stricter with each difference listed; `Docs/install.md`'s statement corrected | any difference between the implementations fails CI; a one-character mutation of the validator is caught; the harness drives the syntax vectors through module 1's real API and the suppressions table | OPEN | — |
| A-end | **Part A close:** module 1 CI green on its head, the platform lock pinned at it, the one `fault-proofs` dispatch of the part, the 13 special cases listed | `check-verified-heads` OK | OPEN | — |

## Part B — Module 2 (`coldmail-send-managed`)

| # | Item | Exit check | Status | Evidence |
|---|---|---|---|---|
| B1 | **The worker never gives up.** It reconnects forever with a capped exponential backoff (dials for the cap and the jitter, documented defaults) | the control is down for longer than the old ~30 attempts, then up: the worker reconnects and sends with no restart (paired control: the same stack with the control up throughout) | OPEN | — |
| B2 | **Forged cursor:** a bad or forged cursor answers a 400 with the envelope, never a 500 | a test per paginated route with a paired control (a real cursor still pages) | OPEN | — |
| B3 | **Plain-text part.** An optional `text` field: when present the message is `multipart/alternative` (text, then HTML); when absent the output is byte-identical to today; a retry renders identically (the duplicate guarantee) | a golden test of today's bytes for the absent case, a structure test for the present case, a retry-renders-identically test | OPEN | — |
| B4 | **`attempt_count`:** documented as what it actually counts; an accurate field only if the platform needs one (it does not today) | the documentation says what a test shows | OPEN | — |
| B-end | **Part B close:** module 2 CI green on its head, the platform lock pinned at it, the one `fault-proofs` dispatch of the part | `check-verified-heads` OK | OPEN | — |

## Part C — Module 3 (`coldmail-send-byo`)

| # | Item | Exit check | Status | Evidence |
|---|---|---|---|---|
| C1 | **Cancel (OD-1).** `POST /v1/cancel` mirroring module 2: not-yet-claimed items become `cancelled`, in-flight items finish and are reported once, idempotent, tenant-scoped; the charge row (free; module 3 is not metered); the platform adapter method; the test that held `Cancel` absent removed | module tests incl. a cross-tenant refusal and a second cancel; the harness proves nothing new is sent after the cancel (at the sink) | OPEN | — |
| C2 | **409 causes (OD-3).** A `details[]` code on the `run` 409 (`suspended`, `campaign_active`, `campaign_cap`, `queue_cap`); quote's two wrong predictions fixed; the platform maps each cause so "cause unknown" happens only for an older module | one test per cause with a paired control; quote and run agree on every cause | OPEN | — |
| C3 | **Internal text leaks.** The test button, bulk-add `code` and `from_alignment.error` return user-vocabulary codes only; internal slugs and raw errors go to logs; a gate fails if any response field on those routes can carry raw error text, with a fault patch | a test per route that forces each internal error and sees only the closed vocabulary; the gate's fault proof | OPEN | — |
| C4 | **SMTP host destination guard (security).** Loopback, private, link-local, cloud-metadata and the other special-use ranges are refused at add time AND at connect time (resolve, then check every address; DNS rebinding guarded by checking the address actually dialled); module 1's probe-destination guard is the model, with a config allowlist for tests | each blocked range refused at both points, a public address allowed (paired control), a rebinding fixture refused at connect | OPEN | — |
| C5 | **Plain-text part:** as B3 (optional `text`, `multipart/alternative`, byte-identical when absent) | as B3 | OPEN | — |
| C6 | **`describe.schemas`:** `results` corrected to `items` so `describe` matches what the handlers return | a test compares `describe` against real responses | OPEN | — |
| C-end | **Part C close:** module 3 CI green on its head, the platform lock pinned at it, the one `fault-proofs` dispatch of the part | `check-verified-heads` OK | OPEN | — |

## Part D — Platform

| # | Item | Exit check | Status | Evidence |
|---|---|---|---|---|
| D1 | **Pin the new module heads;** the contract drift gate passes with re-recorded fixtures for every changed route; module 1's 170 KB results fixture shrunk by recording a smaller harness job, never by editing | compare-mode harness green; each changed route's fixture re-recorded from the real module; the large fixture replaced by a recording | OPEN | — |
| D2 | **Module 3 From-gate ON in the harness** with fixture DNS (SPF, DKIM, DMARC): the pass, WARN and BLOCK paths end to end through the adapter; the user-facing codes clean (C3) | the three paths each observed through the adapter, each with its paired control | OPEN | — |
| D3 | **Module 3 events (OD-10).** One enforced health action driven for one account in the harness so the events page is observed non-empty; its contract recorded and the allowlist entry removed | the events page contract is exercised (an allowlist entry for an exercised contract is red) | OPEN | — |
| D4 | **`modclient.Health`:** the liveness branch (a 200 whose status is not ok) in the no-value table, with a fault patch | the mutation of that branch is caught; the fault proof | OPEN | — |
| D5 | **Adapters:** module 3 `Cancel` and the 409 cause mapping; module 1 `describe`, the idempotent create and the totals; module 2/3 plain-text pass-through; unit tests and live harness tests for each | the live harness tests pass; the contract drift gate covers every new route | OPEN | — |
| D6 | **Owner rulings recorded:** OD-5, OD-6/8, OD-7 | the open-decision rows below CLOSED | OPEN | — |
| D-end | **Part D close:** every platform workflow green on the code head, one `fault-proofs` dispatch, the parent re-pinned | `check-verified-heads` OK; `check-modules-lock` OK | OPEN | — |

## Part E — Housekeeping

| # | Item | Exit check | Status | Evidence |
|---|---|---|---|---|
| E1 | **The one authorised GitHub deletion:** the scratch branches Claude Code created for runner iteration in the five repositories, listed first, each confirmed merged into `main` or free of any commit missing from `main` that matters (the reason written down per branch); never `main`, never a tag, never anything in `zexSites`; a branch of unclear status is kept and reported | the list and the reason per branch are in this row; the remaining branches are exactly `main` plus the kept ones | OPEN | — |
| E2 | **Docs:** the build plan's status table, Money section, `Docs/install.md` and each module's docs current; every open decision below closed as CLOSED (ruling, commit, run) | `check-verified-heads` OK on the documents | OPEN | — |
| E3 | **The "not checked" list reprinted:** every entry names the later phase whose exit criterion covers it, or is an owner action | the list is in this document | OPEN | — |
| E-end | **Close:** every row CLOSED with a verified head and run, module CIs green, modules pinned, the platform's eight workflows green on the code head, the parent re-pinned | `check-verified-heads` OK; `check-modules-lock` OK | OPEN | — |

## The open decisions of Phase 3, each closed here

| # | Decision | What closes it | Status | Evidence |
|---|---|---|---|---|
| OD-1 | Module 3 has no cancel | C1, D5 | OPEN | — |
| OD-2 | One caller key per platform user | A4 | OPEN | — |
| OD-3 | Module 3's run 409 has four indistinguishable causes | C2, D5 | OPEN | — |
| OD-4 | Module 1 node promotion | A2 | OPEN | — |
| OD-5 | Module 2's 409 is backpressure; phase 4 owns serialisation | D6 (ruling) | OPEN | — |
| OD-6 | The Tier 4 decision | A1, D6 (ruling: partial Tier 4) | OPEN | — |
| OD-7 | Where the service container gets module 3's token | D6 (the dial exists and has a consumer) | OPEN | — |
| OD-8 | Tier 4 evidence | A1, D6 | OPEN | — |
| OD-9 | How strict the platform's own address check is | A5 | OPEN | — |
| OD-10 | Module 3 account events never observed non-empty | D3 | OPEN | — |

## The "things noticed" of Phase 3, where each is closed

| Noticed | Closed by |
|---|---|
| Module 1 re-registers its node as `provisioning` at every boot; crash recovery waits for the lease TTL | A2 |
| Module 1 has no `describe`; a non-UUID job id is a 500; bulk POST not idempotent; the results of a running job are a moving target | A1, A3 |
| Module 1 charges an address that normalises but fails its syntax layer (M1-10); `install.md` "one rule" inexact (22 vectors) | A5 |
| Module 2's worker gives up after about 30 reconnects | B1 |
| Module 2's forged cursor answers 500; `attempt_count` is not an attempt count | B2, B4 |
| Module 2's quote validates nothing about capacity | E3 (the platform's job-creation pre-flight, phase 4) |
| Module 3's test button, bulk-add and `from_alignment.error` carry internal slugs | C3 |
| Module 3's `smtp_host` accepts any IP | C4 |
| Module 3's body is one `text/html` part; module 2's likewise | B3, C5 |
| Module 3's `describe.schemas` says `results` | C6 |
| `modclient.Health`'s liveness branch is not in the no-value table | D4 |
| Module 1's recorded results page is 170 KB | D1 |
| The recorded fixtures keep the case names of the contracts (shape families) | E3 (documented by each package's README; the matcher decides by structure) |
| A stale populated fixture off by two or more keys can fall to an empty-page contract | E3 |
| The gates cannot see a SIGKILL mid-write, module 3's From-gate WARN and BLOCK paths, or anything only production does | D2 (From-gate); E3 (the rest) |

## The "not checked" list

What the gates of Phase 3 and of this close-out do **not** check, reprinted from `phase-3-findings.md` section 7 and from what the close-out found, **each with the later phase whose exit criterion covers it or the owner action that does**. The drift gate still prints its own list on every run; this is the same list with an owner for every line. Nothing on it is open work of Phase 3.

| Not checked | Covered by |
|---|---|
| A SIGKILL of a module (or of the platform) in the middle of a write; module 1's leased batch after a SIGKILL (the proof is a graceful stop) | **P4** exit criteria "a platform crash mid-job resumes with no item lost, duplicated or double-charged" and "a module outage halts polling cleanly and invents no outcome"; **P8** "the panic switch halts every module losslessly" and the restore drill |
| Responses for inputs the harness never sends; fields a module adds in states the harness never reaches; anything a module does only in production | **P8** (load test at realistic job sizes, the security review of the public surfaces) and the **owner action** "the real-provider smoke test for both senders" |
| The port-25 behaviour of a real node (the harness's node has no outside network; `port25_open` comes from the audit of the harness's own stand-in) | **owner action** "the port-25 host for module 1" |
| A module restarted by an operator with a different configuration (a dial changed between two runs) | **P9** "a clean install reproduced from the docs alone" and the go-live checklist; the module's own start-up refusals are the first line |
| The `rate_limited` answers, unless a test reaches them | **P4** (the job engine's retry and backoff on `rate_limited` is the first caller that can meet it) |
| The contracts the harness cannot elicit, each with its reason in `m1DriftNotElicited` (3: `POST /v1/verify/bulk` `paused`, `unavailable`, `internal_error`), `m2DriftNotElicited` (1: the forged-cursor 500, which module 2 no longer answers since B2; module 3 still does and elicits the shared contract) and `m3DriftNotElicited` (2: the events page with a next cursor and a pause event, `POST /v1/run` `internal_error`; the test button's `unavailable` left the list when `TestM3Adapter_TestButtonSaysUnavailableWhenTheCredentialCheckCannotRun` began to elicit it on purpose with a damaged stored credential: before that it was exercised only by chance, which turned the gate red either way) | **P4** for the answers a job in flight can meet (`paused`, `unavailable`, `internal_error`: the job engine is the first caller that must react to them); **P8** for the rest (a fault-injecting stand-in for each module belongs to the outage drills, and the events page needs an account the real health loop has paused); an entry for a contract that WAS exercised stays red |
| Fixtures of the contracts the harness did not reach are hand-written from module source (module 3: three, `run.error.internal_error`, `accounts.test.unavailable` (now elicited on purpose, not yet recorded), `accounts.events.page`; each package's `testdata/README.md` says which) | **P8** (re-recorded the first time the real module is made to answer them); until then the matcher compares them by structure only and the README says so |
| The recorded fixtures keep the case names of the contracts: a name is a **shape family**, not a promise about the content (a file named `ok` can hold the answer of a quarantined account) | documented by each package's README, which lists every file whose content differs from its name; the matcher decides by structure, never by name |
| A stale populated fixture that is off by two or more keys can fall through to an empty-page contract (the matcher's nearest-contract limitation) | the matcher reports the nearest miss and the harness records every shape the real module answers; a stale recording is red as soon as the module's answer changes (the drift gate) |
| Module 2's quote validates nothing about capacity | **P4**, the platform's job-creation pre-flight (the platform's own facts decide before module 2 is asked) |
| Module 3's forged cursor on the events page still answers 500 (the module's own finding; the platform's adapter treats it as `Internal`) | **owner decision** (an additive 400, the same ruling as B2; not on the critical path: the platform never builds a cursor itself) |
| The platform's money and entitlement paths under a network partition between two platform instances | **P8** (the leader-role exit criterion added by the Phase 2 close-out, E4) |
| Settlement of a job far larger than the harness's | **P8** (the load test, "settlement matches the module report exactly, item by item") |

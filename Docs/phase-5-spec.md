# Phase 5 — Owner decisions closed, then Templates and rendering: spec

Roadmap: [`coldmail-platform-build-plan.md`](coldmail-platform-build-plan.md). Previous: [`phase-4-spec.md`](phase-4-spec.md) (platform code `5c0f2da`, docs `8ec7e3e`; verify `c516f28`, send-managed `d023fdb`, send-byo `59b2d2e`; parent `039f28a`). One phase, one session, one report: nothing is deferred to a close-out. Evidence is run ids only; the verified head and its eight runs are cited once, here, when the phase ends.

**Verified head:** _filled at the end (all eight workflows on one SHA)._

## Chunks

| # | Chunk | Exit check | Status | Evidence |
|---|---|---|---|---|
| S1 | `internal/render`: the grammar (merge fields, nestable spintax, escapes, `{{` always a field), compile with error positions, seeded render (values literal, HTML-escaped in HTML, CR/LF stripped in the subject), HTML to text | unit and property tests: determinism, literal values, escapes, positions | OPEN | — |
| S2 | Templates in jobs: syntax checked at save, missing-field count and list and sample renders at preview, rows skipped free at confirm (`P-09`), `TemplateRenderer` seeded by `hash(job_id, item_id)` | db-tests | OPEN | — |
| S3 | Unsubscribe: signed per-item token, public `GET`/`POST /u/{token}` (confirm page, RFC 8058 one-click), rate limit, `internal/suppress`, footer link unless the template places `{{unsubscribe_url}}`, `public.job_items.unsubscribe_url` | db-tests + unit; forged token refused | OPEN | — |
| S4 | Modules 2 and 3: `List-Unsubscribe` and `List-Unsubscribe-Post` headers from `unsubscribe_url` (additive), the owner's checklist rows and records | each module's CI | OPEN | — |
| S5 | Module 1: a second served policy without the `external` block, TLS rows in its checklist, stale entries removed | module 1 CI | OPEN | — |
| S6 | Vendor opt-out: per-user setting (default opted in), API, the job's snapshot of it, reserve at the non-vendor price, the no-external policy sent to module 1 (1:1) | db-tests + a stage of the `jobs` scope | OPEN | — |
| S7 | Paused job auto-cancel after `PLATFORM_JOB_PAUSE_MAX_DAYS` (7): unsent cancelled and free, credits released, slot freed, event | db-tests (short dial) | OPEN | — |
| S8 | Content retention: rendered subject, body, text and the stored link cleared 7 days after the job ended; outcomes kept | db-tests | OPEN | — |
| S9 | Pin the modules; live proofs at the sink: byte-identical renders across retries, a SIGKILL and two instances, a missing field never sent, injection rendered literally, unsubscribe link and one-click then excluded, headers on real messages (modules 2 and 3) | scope runs + full harness | OPEN | — |
| S10 | Owner decisions recorded and the small ones built (M3 expiry 30 days, retention 7 days, accepted-risk record, TLS edge, promotion dates); `OWNER-ACTIONS.md` rewritten short; build plan: P8 and P9 merged | `platform-check`, `gates` | OPEN | — |
| S11 | One review pass over the money, send and security changes | findings fixed | OPEN | — |
| S12 | Final verification (eight workflows once on the final code head), build plan P5 done, parent re-pinned | `check-verified-heads` | OPEN | — |

## Decisions taken (one line each; none is irreversible)

1. **Grammar.** Merge field `{{name}}` / `{{name|fallback}}` (name `[A-Za-z_][A-Za-z0-9_]*`, at most 64); spintax `{a|b|c}` with at least two options, nestable to depth 8; escapes `\{ \} | \\`; `{{` always opens a field; a stray `}` or an unclosed group is an error with its position (a brace for CSS or script must be escaped). Reserved fields: `email` and `unsubscribe_url`.
2. **Missing means absent or empty after trimming**; a field with a fallback never counts as missing; the fallback is template text (not escaped).
3. **Seed** = first 8 bytes of SHA-256(`job_id` + `:` + `item_id`); `item_id` is the row id the platform already hands every module, so a preview sample is exactly what will be sent. HTML and the text part share one spin stream, the subject has its own; the choice generator is implemented in the package (no library whose algorithm can change).
4. **Plain text:** an explicit text template is rendered; a blank one is generated from the rendered HTML (links kept as `text (url)`), so every message has a text part.
5. **Unsubscribe token** is `base64url(item_id (16 bytes) || HMAC-SHA256(secret, "unsub|v1|" + item_id)[:24])`: short, no address in the URL, no enumeration (a bad MAC and an unknown item answer alike). `GET` shows a confirm page and changes nothing (link prefetchers); `POST` (the form, or the RFC 8058 one-click body) suppresses. Both need `PLATFORM_UNSUBSCRIBE_SECRET` (at least 32 bytes) and `PLATFORM_PUBLIC_BASE_URL` (https); without them a send job cannot be confirmed (a send without a working link is not allowed).
6. **Modules carry the headers through one additive nullable column** `public.job_items.unsubscribe_url` (a single https URL, never a free header map: no header injection surface); each sender emits `List-Unsubscribe: <url>` and `List-Unsubscribe-Post: List-Unsubscribe=One-Click` when it is set.
7. **Retention placeholders:** after 7 days `rendered_subject` and `rendered_body` become `[removed]` (the modules' columns are NOT NULL), `rendered_text` and `unsubscribe_url` become NULL; the link in a sent message keeps working (it is derived from the item id).
8. **Vendor opt-out** is a per-user setting, default opted in, read once into the job when it is created (a later change never alters a job that exists); an opted-out job reserves at the non-vendor price and module 1 receives the policy without the `external` block.
9. **Pause limit:** a job paused from running more than `PLATFORM_JOB_PAUSE_MAX_DAYS` (7) is cancelled by the engine through `cancelling` (so the module cancels its unclaimed items); the clock is the database's.
10. **Owner rulings recorded:** M3 `campaign_expiry_sec` 30 days; `debug_log_retention_days` 7 and the promotion dates (retention +14 days, account purge +14 more, shadow modes after 14 quiet days) are checklist rows; master key without rotation accepted for V1 (a record for the owner to sign); TLS terminates at one public Caddy edge in front of the platform API and web app only, the modules private; M1 failure-origin section 4 cases stay `undetermined`; pause and resume wording stays as it is; disclosure texts are drafted in P8.

## Not checked (named owner or phase)

Real mailbox providers and their handling of one-click unsubscribe (owner: real-provider smoke test, `OWNER-ACTIONS.md`); the public edge, DNS and TLS (P8); the customer screens for the opt-out and the preview (P6).

// EVERY request and response shape of the platform API, written as types. This file is the contract between the screens and the Go service.
//
//   Part 1: what exists today, from Docs/jobs-api.md and internal/api/views.go (verified against the code on 2026-11-28).
//   Part 2: what Phase 6 adds, from the "New API" table of Docs/phase-6-spec.md AND the sections of Docs/jobs-api.md the route builders wrote ("The account", "The
//           template check", "Suppressions", "Module 3 accounts"), read on 2026-11-28. The routes are still being built: if a real answer differs, fix this file,
//           api/endpoints.ts and copy/errors.ts together (copy.test.ts holds the error codes of the document equal to the catalogue).
//
// Conventions of the API: ids are lower-case uuid strings, times are RFC 3339 strings in UTC, a value that does not apply is null (never ""), money is whole credits.

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// Part 1: the job API (exists)
// ---------------------------------------------------------------------------------------------------------------------------------------------------

/** m1 = verify, m2 = send through the platform's servers, m3 = send through the user's own SMTP accounts. */
export type ModuleId = 'm1' | 'm2' | 'm3'

export type JobStatus =
  | 'draft'
  | 'previewed'
  | 'confirmed'
  | 'scheduled'
  | 'running'
  | 'paused'
  | 'cancelling'
  | 'completing'
  | 'succeeded'
  | 'cancelled'
  | 'expired'
  | 'failed'

/** The statuses `GET /v1/jobs?status=active` returns (spec, "New API"). */
export const ACTIVE_JOB_STATUSES = ['scheduled', 'running', 'paused', 'cancelling'] as const satisfies readonly JobStatus[]

/** The statuses a job ends in: nothing moves it after these. */
export const FINAL_JOB_STATUSES = ['succeeded', 'cancelled', 'expired', 'failed'] as const satisfies readonly JobStatus[]

export type UnknownChoice = 'include' | 'exclude'

export interface JobSettings {
  /** null until the user says include or exclude (never defaulted) */
  unknown_choice: UnknownChoice | null
  /** a verify job only (absent on a send job): the user's vendor opt-out as it was when the job was created */
  vendor_opt_out?: boolean
}

/** The subject, HTML and text of a send job are templates (Docs/templates.md). */
export interface JobContent {
  subject: string
  html: string
  text: string
  reply_to: string
}

/** One sample message of a preview: a real row rendered as it will be sent. Only in the answer of POST /v1/jobs/{id}/preview, never stored, never on a GET. */
export interface Sample {
  ordinal: number
  subject: string
  html: string
  text: string
}

export interface Preview {
  rows: number
  pending: number
  malformed: number
  /** malformed rows by reason word (an object, never null) */
  malformed_reasons: Record<string, number>
  duplicates: number
  excluded: number
  suppressed: number
  /** true for a verify job: its suppressed rows are verified and charged anyway */
  suppressed_still_verified: boolean
  missing_fields: number
  missing_field_names: string[]
  missing_field_names_truncated: boolean
  unrenderable: number
  /** the rows that would reach the module */
  units: number
  /** a chained send: the source job's results by verdict */
  verdicts?: Record<string, number>
  selected: number
  unknown_choice: UnknownChoice | null
  unknown_count: number
  unknown_choice_required: boolean
  /** false for module 3 (entitlement, no credits) */
  metered: boolean
  rate_unit?: string
  unit_cost: number
  /** what a confirm would hold */
  credits: number
  available: number
  shortfall: number
  vendor_reachable_known: boolean
  vendor_opt_out: boolean
  /** names everything the numbers depended on: a confirm of a stale one is 409 preview_stale */
  fingerprint: string
  computed_at: string
  /** present only in the answer of the preview route */
  samples?: Sample[]
}

export interface Job {
  id: string
  module: ModuleId
  status: JobStatus
  source_job_id: string | null
  scheduled_start: string | null
  start_deadline: string | null
  paused_from: JobStatus | null
  settings: JobSettings
  /** a send job only, and not in a list */
  content?: JobContent
  preview: Preview | null
  preview_fingerprint: string | null
  previewed_at: string | null
  confirmed_at: string | null
  started_at: string | null
  finished_at: string | null
  cancel_requested_at: string | null
  /** set when the retention cleared the template, the merge data and the rendered messages (Phase 6 A12; absent from an older service) */
  content_cleared_at?: string | null
  created_at: string
  updated_at: string
}

/** The answer of every route that returns one job (get, preview, the five moves). */
export interface JobEnvelope {
  job: Job
}

export interface JobList {
  jobs: Job[]
  next_cursor: string | null
}

// ---- create ----

export interface CreateJobRow {
  email: string
  /** values are strings */
  fields?: Record<string, string>
}

/** `POST /v1/jobs` with a JSON body. A send job needs `content`; a chained send names `source_job_id` and sends no `rows`. */
export interface CreateJobJson {
  module: ModuleId
  rows?: CreateJobRow[]
  content?: Partial<JobContent> & Pick<JobContent, 'subject' | 'html' | 'text'>
  source_job_id?: string
  settings?: { unknown_choice?: UnknownChoice }
}

/** `POST /v1/jobs` with a `text/csv` body: everything but the rows travels in the query (64 KiB of query in all). */
export interface CreateJobCsvQuery {
  module: ModuleId
  source_job_id?: string
  unknown_choice?: UnknownChoice
  subject?: string
  html?: string
  text?: string
  reply_to?: string
  /** the header of the column that holds the address (default `email`); every other column is a merge field (spec, decision 6) */
  email_column?: string
}

// ---- preview, confirm, moves ----

/** `POST /v1/jobs/{id}/preview`: no body, or this. */
export interface PreviewRequest {
  unknown_choice?: UnknownChoice
}

/** `POST /v1/jobs/{id}/confirm` (the `Idempotency-Key` header is required). */
export interface ConfirmRequest {
  fingerprint: string
  /** RFC 3339, between one minute and 366 days ahead */
  scheduled_start?: string
  unknown_choice?: UnknownChoice
}

export interface ConfirmResult {
  job: Job
  replayed: boolean
  /** credits held (0 for module 3) */
  reserved: number
  pending: number
  suppressed: number
  missing_fields: number
  unrenderable: number
}

// ---- events and results ----

export type JobEventKind = 'created' | 'transition' | 'stale' | 'flag' | 'note'
export type JobEventActor = 'user' | 'admin' | 'engine'

export interface JobEvent {
  id: number
  kind: JobEventKind
  from_status: JobStatus | null
  to_status: JobStatus | null
  actor_kind: JobEventActor
  /** a word or a sentence of the platform's: never shown raw */
  reason: string
  created_at: string
}

export interface EventsPage {
  events: JobEvent[]
  /** pass as `after` for the next page; null on the last */
  next_after: number | null
}

/** Who decided a result: the module that did the work, or the platform itself. */
export type ResultOrigin = 'module' | 'platform'
export type ResultDecision = 'charge' | 'free' | 'unmapped'

export interface JobResult {
  /** the row's position, from 1 */
  ordinal: number
  email: string
  origin: ResultOrigin
  /** valid, risky, invalid, unknown, sent, bounced, failed, and the platform's own: malformed, duplicate, suppressed, excluded, missing_field, unrenderable, cancelled, expired... (see ui/status.ts) */
  state: string
  /** a word of the module's or the platform's (copy/words.ts resultReasonText); for `missing_field` the user's own field names, comma separated */
  reason: string | null
  decision: ResultDecision
  unit: string | null
  rule: string
  recorded_at: string
  /** the address was on the user's suppression list when the job was confirmed */
  suppressed: boolean
}

export interface ResultsPage {
  results: JobResult[]
  next_cursor: string | null
}

// ---- settings ----

export interface UserSettings {
  /** true: the user's verify jobs are created without the paid vendor fallback */
  vendor_fallback_opt_out: boolean
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// Part 2: what Phase 6 adds (Docs/phase-6-spec.md "New API" and the new sections of Docs/jobs-api.md)
// ---------------------------------------------------------------------------------------------------------------------------------------------------

/** `GET /v1/me` */
export interface Me {
  email: string
}

export interface ModuleBalance {
  available: number
  reserved: number
}

export type M3EntitlementState = 'active' | 'suspended' | 'unknown'

export interface M3Entitlement {
  enabled: boolean
  /** RFC 3339, or null when it does not expire or is not enabled */
  until: string | null
  state: M3EntitlementState
}

/** `GET /v1/balances` */
export interface Balances {
  modules: { m1: ModuleBalance; m2: ModuleBalance }
  m3: M3Entitlement
}

/** `GET /v1/jobs` accepts `?status=active` besides cursor and limit. */
export interface ListJobsQuery {
  cursor?: string
  limit?: number
  status?: 'active'
}

/** `POST /v1/templates/check` */
export interface TemplateCheckRequest {
  subject: string
  html: string
  text: string
}

export interface TemplateField {
  name: string
  has_fallback: boolean
}

export interface TemplateCheckError {
  part: 'subject' | 'html' | 'text'
  line: number
  column: number
  /** the template package's own sentence about the user's own template: the one server text a screen may show */
  message: string
}

/** Status 200 either way. */
export type TemplateCheck =
  | { ok: true; fields: TemplateField[] }
  | { ok: false; errors: TemplateCheckError[] }

/** Where a suppression came from: the recipient asked (`unsubscribe`), the user added it (`manual`) or it came in a file (`import`). The list only grows. */
export type SuppressionSource = 'unsubscribe' | 'manual' | 'import'

export interface Suppression {
  email: string
  source: SuppressionSource
  created_at: string
}

/** `GET /v1/suppressions?q=&cursor=&limit=` (limit 1 to 100, default 50; `q` is a search, at most 254 bytes). */
export interface SuppressionsQuery {
  q?: string
  cursor?: string
  limit?: number
}

export interface SuppressionsPage {
  entries: Suppression[]
  next_cursor: string | null
}

/** `POST /v1/suppressions`: 201 `{entry}`, or 200 `{entry, already: true}`; an invalid address is `400 validation_failed` with `details.address_reason`. */
export interface AddSuppressionResponse {
  entry: Suppression
  already?: boolean
}

/** `POST /v1/suppressions/remove` (the removal is recorded in an append-only history). `false`: the user had no such entry. */
export interface RemoveSuppressionResponse {
  removed: boolean
}

/** The closed list of why an address was refused (`address_reason` of a 400, `reason` of an invalid import line). It only grows: copy/words.ts malformedReasonText shows an unknown one as "not valid". */
export type AddressReason =
  | 'empty'
  | 'no_at'
  | 'multiple_at'
  | 'empty_local'
  | 'empty_domain'
  | 'bad_idna'
  | 'part_empty'
  | 'local_too_long'
  | 'domain_too_long'
  | 'domain_no_dot'
  | 'cannot_store'
  | 'bad_line'
  | 'invalid_address'

export interface SuppressionImportInvalid {
  /** the line of the file (the header is line 1) */
  line: number
  reason: AddressReason
}

/** `POST /v1/suppressions/import` with a `text/csv` body that has an `email` column. `invalid_sample` holds the first 20 invalid lines; no address is ever echoed. */
export interface SuppressionImportResult {
  added: number
  already_there: number
  invalid: number
  invalid_sample: SuppressionImportInvalid[]
}

/** The verdict of module 3's From-gate on a From address, as a word (never the module's sentence). */
export type FromGateState = 'unverified' | 'aligned' | 'unaligned_warned' | 'blocked' | 'indeterminate'
export type M3AccountStatus = 'active' | 'paused' | 'quarantined' | 'disabled'
export type M3PauseReason = 'daily_limit' | 'throttle' | 'manual' | 'unrecognised'
export type M3QuarantineReason = 'auth_failure' | 'consecutive_failures' | 'failure_rate' | 'manual' | 'unrecognised'
export type M3Verification = 'unverified' | 'verified' | 'failed'
export type M3VerifyCode =
  | 'verify_connect_failed'
  | 'verify_timeout'
  | 'verify_tls_unavailable'
  | 'verify_tls_failed'
  | 'verify_auth_failed'
  | 'verify_protocol_error'
  | 'verify_connection_lost'
  | 'verify_host_not_allowed'
  | 'verify_error'
  | 'verification_unavailable'

/** One of the user's own SMTP sender accounts at module 3. No secret ever: `has_credential` only. A `blocked` From address is a successful add whose `from_gate` says so. */
export interface M3Account {
  id: string
  smtp_host: string
  smtp_port: number
  smtp_username: string
  from_address: string
  from_name: string | null
  daily_limit: number
  has_credential: boolean
  /** active, paused, quarantined, disabled (in that order of precedence: disabled, quarantined, paused) */
  status: M3AccountStatus
  pause_reason: M3PauseReason | null
  quarantine_reason: M3QuarantineReason | null
  verification: M3Verification
  /** why a check failed, a closed word of module 3's; null when it did not */
  verify_code: M3VerifyCode | null
  from_gate: FromGateState
  created_at: string
}

/** `GET /v1/m3/accounts?cursor=`: oldest account first. */
export interface M3AccountsPage {
  accounts: M3Account[]
  next_cursor: string | null
}

/**
 * `POST /v1/m3/accounts`. The first four are required (non-blank, at most 1024 bytes each). A From address whose verdict is `unaligned_warned` is not added
 * until `acknowledge_from_gate` is true (`422 from_gate_ack_required`). The answer is 201 whether or not the credential check passed: read `verification`.
 */
export interface M3NewAccount {
  smtp_host: string
  smtp_username: string
  /** the SMTP password or app key: sent once, never returned, in no log */
  smtp_secret: string
  from_address: string
  smtp_port?: number
  from_name?: string
  daily_limit?: number
  acknowledge_from_gate?: boolean
}

export interface M3AccountResponse {
  account: M3Account
}

/** `POST /v1/m3/accounts/{id}/test`: the body is optional. */
export interface M3TestRequest {
  /** acknowledges a From-gate warning the click finds */
  acknowledge?: boolean
}

/** How far the test got: done, connect (no connection, or lost), host (the module refuses the host), tls, login (the provider refused the credentials), protocol, unavailable (the check could not run). */
export type M3TestStage = 'done' | 'connect' | 'host' | 'tls' | 'login' | 'protocol' | 'unavailable'

export interface M3TestResponse {
  result: {
    /** true when the credential was verified. A failed credential is still a 200. */
    ok: boolean
    stage: M3TestStage
    from_gate: FromGateState
  }
}

/** `POST /v1/m3/accounts/bulk`: `{ accounts }` as JSON (at most 500 and 1 MiB), or a `text/csv` body whose header names the columns of an add. */
export interface M3BulkRequest {
  accounts: M3NewAccount[]
}

/** The word of one line of a bulk add: the first two are successes (the second: the account exists and its credential is not verified: read the list). */
export type M3BulkCode =
  | 'created'
  | 'created_unverified'
  | 'account_duplicate'
  | 'from_gate_ack_required'
  | 'host_forbidden'
  | 'account_limit_reached'
  | 'validation_failed'
  | 'account_action_unavailable'
  | 'not_found'
  | 'internal_error'

export interface M3BulkResponse {
  /** in order; `line` is the row's number in the JSON list or its line in the CSV (the header is line 1) */
  results: Array<{ line: number; ok: boolean; code: M3BulkCode }>
}

/** `POST /v1/m3/accounts/{id}/remove`: module 3's soft delete. */
export interface M3RemoveResponse {
  removed: true
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// The service's own config document and the error envelope
// ---------------------------------------------------------------------------------------------------------------------------------------------------

/** `GET /config.json`: public values only (the Supabase URL and the public anon key). */
export interface AppConfig {
  supabase_url: string
  anon_key: string
}

/** The error envelope as sent: `message` is for developers and is never read by the app (api/errors.ts). */
export interface ApiErrorEnvelope {
  error: {
    code: string
    message: string
    request_id: string
    details?: Record<string, unknown>
  }
}

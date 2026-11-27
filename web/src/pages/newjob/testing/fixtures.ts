// Data and helpers for the tests of this area. Test code only: nothing of the app imports this folder (a test reads the sources of the area and skips it).
import { expect } from 'vitest'
import type { ConfirmResult, Job, M3Account, Preview, Sample } from '../../../api/types.ts'

export function preview(over: Partial<Preview> = {}): Preview {
  return {
    rows: 5,
    pending: 3,
    malformed: 1,
    malformed_reasons: { no_at: 1 },
    duplicates: 1,
    excluded: 0,
    suppressed: 1,
    suppressed_still_verified: true,
    missing_fields: 0,
    missing_field_names: [],
    missing_field_names_truncated: false,
    unrenderable: 0,
    units: 3,
    selected: 0,
    unknown_choice: null,
    unknown_count: 0,
    unknown_choice_required: false,
    metered: true,
    rate_unit: 'm1_own',
    unit_cost: 1,
    credits: 3,
    available: 100,
    shortfall: 0,
    vendor_reachable_known: true,
    vendor_opt_out: false,
    fingerprint: 'fp-1',
    computed_at: '2026-12-01T09:00:00.123456Z',
    ...over,
  }
}

export function job(over: Partial<Job> = {}): Job {
  const p = over.preview === undefined ? preview() : over.preview
  return {
    id: 'job-1',
    module: 'm1',
    status: 'previewed',
    source_job_id: null,
    scheduled_start: null,
    start_deadline: null,
    paused_from: null,
    settings: { unknown_choice: null, vendor_opt_out: false },
    preview: p,
    preview_fingerprint: p === null ? null : p.fingerprint,
    previewed_at: '2026-12-01T09:00:00Z',
    confirmed_at: null,
    started_at: null,
    finished_at: null,
    cancel_requested_at: null,
    created_at: '2026-12-01T08:59:00Z',
    updated_at: '2026-12-01T09:00:00Z',
    ...over,
  }
}

/** A send job as the preview route answers it: its message and the samples. */
export function sendJob(over: Partial<Job> = {}, previewOver: Partial<Preview> = {}): Job {
  return job({
    id: 'job-send',
    module: 'm2',
    settings: { unknown_choice: null },
    content: { subject: 'Hello', html: '<p>Hi</p>', text: '', reply_to: '' },
    preview: preview({ rate_unit: 'm2', metered: true, unit_cost: 1, vendor_reachable_known: false, ...previewOver }),
    ...over,
  })
}

export function samples(): Sample[] {
  return [
    { ordinal: 1, subject: 'Hello Ann', html: '<p>Hi Ann</p>', text: 'Hi Ann' },
    { ordinal: 3, subject: 'Hello Bo', html: '<p>Hi Bo</p>', text: 'Hi Bo' },
    { ordinal: 5, subject: 'Hello Cy', html: '<p>Hi Cy</p>', text: 'Hi Cy' },
  ]
}

export function confirmResult(j: Job, over: Partial<ConfirmResult> = {}): ConfirmResult {
  return { job: { ...j, status: 'confirmed', confirmed_at: '2026-12-01T09:05:00Z' }, replayed: false, reserved: 3, pending: 3, suppressed: 0, missing_fields: 0, unrenderable: 0, ...over }
}

export function account(over: Partial<M3Account> = {}): M3Account {
  return {
    id: 'acc-1',
    smtp_host: 'smtp.example.test',
    smtp_port: 587,
    smtp_username: 'me',
    from_address: 'me@example.test',
    from_name: null,
    daily_limit: 100,
    has_credential: true,
    status: 'active',
    pause_reason: null,
    quarantine_reason: null,
    verification: 'verified',
    verify_code: null,
    from_gate: 'aligned',
    created_at: '2026-11-01T00:00:00Z',
    ...over,
  }
}

export const FAR_FUTURE_ENTITLEMENT = '2099-01-01T00:00:00Z'

export function csvFile(text: string, name = 'list.csv'): File {
  return new File([text], name, { type: 'text/csv' })
}

type FetchFn = { mock: { calls: Array<[unknown, RequestInit?]> } }

/** Every call of the mocked fetch to `METHOD /path`, with its URL read. */
export function callsTo(fetchFn: FetchFn, method: string, path: string): Array<{ url: URL; init: RequestInit; headers: Record<string, string> }> {
  return fetchFn.mock.calls
    .map(([input, init]) => ({ url: new URL(String(input), window.location.origin), init: init ?? {} }))
    .filter((c) => (c.init.method ?? 'GET').toUpperCase() === method && c.url.pathname === path)
    .map((c) => ({ ...c, headers: (c.init.headers ?? {}) as Record<string, string> }))
}

/** The JSON body of a call. */
export function bodyOf(call: { init: RequestInit }): Record<string, unknown> {
  return JSON.parse(String(call.init.body)) as Record<string, unknown>
}

/** Words that must never reach a screen: the API's codes and the platform's own sentences. */
export const CODE_WORDS = [
  'preview_stale',
  'insufficient_credits',
  'job_slot_taken',
  'nothing_to_do',
  'unknown_choice_required',
  'validation_failed',
  'not_found',
  'internal_error',
  'module_unavailable',
  'idempotency',
  'fingerprint',
  'unknown_choice_changed',
  'no_entitlement',
]

export function expectNoCodeWords(text: string, extra: string[] = []): void {
  const lower = text.toLowerCase()
  for (const word of [...CODE_WORDS, ...extra]) expect(lower.includes(word.toLowerCase()), `the screen says "${word}"`).toBe(false)
}

/**
 * What stops a page being operated by keyboard alone, as readable lines ([] when nothing does): every control is the browser's own (a link, a button, an input,
 * a select, a text area, a summary) so it is in the tab order and answers Enter, Space and the arrow keys by itself; none is taken out of the tab order or put
 * ahead of the others with a tabindex; none is hidden from assistive technology while still focusable; each has a name; and the only other thing in the tab order is
 * a scrolling region that says what it is (a table, a block of text), which the keyboard needs to scroll it.
 */
export function keyboardProblems(root: ParentNode): string[] {
  const bad: string[] = []
  const native = 'a[href], button, input:not([type=hidden]), select, textarea, summary'
  const describe = (el: Element) => `${el.tagName.toLowerCase()} "${(el.textContent ?? '').trim().slice(0, 30)}"`

  for (const el of root.querySelectorAll<HTMLElement>(native)) {
    const tab = el.getAttribute('tabindex')
    if (tab !== null && Number(tab) !== 0) bad.push(`${describe(el)}: tabindex ${tab} takes it out of order`)
    if (el.closest('[hidden], [aria-hidden="true"]') !== null) bad.push(`${describe(el)}: focusable but hidden from assistive technology`)
    const name = el.getAttribute('aria-label') || el.getAttribute('title') || el.textContent?.trim() || (el as HTMLInputElement).labels?.[0]?.textContent?.trim()
    if (!name) bad.push(`${describe(el)}: no accessible name`)
  }
  for (const el of root.querySelectorAll<HTMLElement>('[role=button], [role=link], [role=checkbox], [role=radio], [role=switch], [role=tab], [role=menuitem]')) {
    if (!el.matches(native)) bad.push(`${describe(el)}: a ${el.getAttribute('role')} that is not a native control`)
  }
  for (const el of root.querySelectorAll<HTMLElement>('[tabindex]')) {
    const tab = Number(el.getAttribute('tabindex'))
    if (tab > 0) bad.push(`${describe(el)}: a positive tabindex`)
    if (tab === 0 && !el.matches(native) && !(el.getAttribute('role') === 'region' && (el.getAttribute('aria-label') ?? '') !== '')) {
      bad.push(`${describe(el)}: in the tab order and not a control or a named scrolling region`)
    }
  }
  return bad
}

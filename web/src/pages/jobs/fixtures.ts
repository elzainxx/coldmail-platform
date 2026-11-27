// Builders of API answers for the tests of the dashboard and the jobs screens (no app code imports this file). Each takes the members the test cares about.
import type { Job, JobEvent, JobResult, Preview } from '../../api/types.ts'

export const JOB_ID = '0b000000-0000-4000-8000-000000000001'

export function makePreview(over: Partial<Preview> = {}): Preview {
  return {
    rows: 5,
    pending: 3,
    malformed: 1,
    malformed_reasons: { no_at: 1 },
    duplicates: 1,
    excluded: 0,
    suppressed: 0,
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
    fingerprint: 'f'.repeat(16),
    computed_at: '2026-12-01T09:00:00Z',
    ...over,
  }
}

export function makeJob(over: Partial<Job> = {}): Job {
  return {
    id: JOB_ID,
    module: 'm1',
    status: 'succeeded',
    source_job_id: null,
    scheduled_start: null,
    start_deadline: null,
    paused_from: null,
    settings: { unknown_choice: null, vendor_opt_out: false },
    preview: makePreview(),
    preview_fingerprint: 'f'.repeat(16),
    previewed_at: '2026-12-01T09:00:00Z',
    confirmed_at: '2026-12-01T09:01:00Z',
    started_at: '2026-12-01T09:02:00Z',
    finished_at: '2026-12-01T09:10:00Z',
    cancel_requested_at: null,
    content_cleared_at: null,
    created_at: '2026-12-01T08:59:00Z',
    updated_at: '2026-12-01T09:10:00Z',
    ...over,
  }
}

/** A job id that is unique by number: `jobId(2)`. */
export function jobId(n: number): string {
  return `0b000000-0000-4000-8000-${String(n).padStart(12, '0')}`
}

export function makeResult(ordinal: number, over: Partial<JobResult> = {}): JobResult {
  return {
    ordinal,
    email: `person${ordinal}@example.test`,
    origin: 'module',
    state: 'valid',
    reason: 'accepted',
    decision: 'charge',
    unit: 'm1_own',
    rule: 'M1-03',
    recorded_at: '2026-12-01T09:05:00Z',
    suppressed: false,
    ...over,
  }
}

export function makeEvent(id: number, over: Partial<JobEvent> = {}): JobEvent {
  return {
    id,
    kind: 'transition',
    from_status: 'confirmed',
    to_status: 'running',
    actor_kind: 'engine',
    reason: 'the engine submitted the job to its module',
    created_at: '2026-12-01T09:02:00Z',
    ...over,
  }
}

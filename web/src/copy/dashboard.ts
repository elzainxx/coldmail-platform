// Copy of the 'dashboard' area: every string a user reads on these screens, keyed 'dashboard.<name>'.
// Owned by the screen agent of this area; keep every key under the 'dashboard.' prefix (the type enforces it).
// Rules (copy.test.ts holds them): customer wording, no internal word, no snake_case token, no API code.
// Dynamic words from the API (a status, a reason) are written once in common.ts or errors.ts; do not copy them here.
import type { AreaCopy } from './types.ts'

export const dashboard = {
  'dashboard.title': 'Dashboard',
  'dashboard.intro': 'Your credits, the jobs in progress and your latest results.',

  // ---- balances ----
  'dashboard.balances.title': 'Your balances',
  'dashboard.balance.m1': 'Verification',
  'dashboard.balance.m2': 'Managed sending',
  'dashboard.balance.m3': 'Your own SMTP',
  'dashboard.balance.available': 'Available',
  'dashboard.balance.reserved': 'Set aside for jobs',
  'dashboard.balance.reservedHint': 'Credits held for jobs that are running or scheduled. What a job does not use comes back.',

  // ---- your own SMTP: switched on until a date, switched off, or suspended ----
  'dashboard.m3.state.on': 'On',
  'dashboard.m3.state.off': 'Off',
  'dashboard.m3.state.suspended': 'Suspended',
  'dashboard.m3.enabledUntil': 'Switched on until {date}.',
  'dashboard.m3.enabled': 'Switched on. It has no end date.',
  'dashboard.m3.ended': 'Your access ended on {date}. Contact support to switch it on again.',
  'dashboard.m3.disabled': 'Not switched on for your account. Contact support to have it enabled.',
  'dashboard.m3.suspended': 'Suspended. Sending through your own accounts is paused for your account. Contact support.',
  'dashboard.m3.unconfirmed': 'We have not been able to confirm its current state yet.',
  'dashboard.m3.noCredits': 'Sending through your own accounts does not use credits.',

  // ---- jobs in progress ----
  'dashboard.active.title': 'In progress',
  'dashboard.active.empty.title': 'No jobs in progress',
  'dashboard.active.empty.body': 'When you start a job, it shows up here while it runs.',
  'dashboard.job.starts': 'Starts {date}',
  'dashboard.job.started': 'Started {date}',
  'dashboard.job.created': 'Created {date}',
  'dashboard.job.finished': 'Finished {date}',

  // ---- the latest results ----
  'dashboard.recent.title': 'Recent results',
  'dashboard.recent.empty.title': 'No finished jobs yet',
  'dashboard.recent.empty.body': 'When a job finishes, it is listed here and you can open its results or download them.',
  'dashboard.recent.all': 'All jobs',
} as const satisfies AreaCopy<'dashboard'>

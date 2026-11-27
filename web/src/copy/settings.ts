// Copy of the 'settings' area: every string a user reads on these screens, keyed 'settings.<name>'.
// Owned by the screen agent of this area; keep every key under the 'settings.' prefix (the type enforces it).
// Rules (copy.test.ts holds them): customer wording, no internal word, no snake_case token, no API code.
// Dynamic words from the API (a status, a reason) are written once in common.ts or errors.ts; do not copy them here.
//
// THE DISCLOSURE. The spec calls the disclosure of the vendor opt-out `disclosure.vendor.settings`; the catalogue holds every key of a file under that file's own
// prefix (copy.test.ts), so it is `settings.disclosure.vendor` here. Its wording is a PLACEHOLDER until P8 drafts the customer wordings with the terms: it says
// plainly what the setting changes and nothing more (the same sentence appears again where a verify job is previewed, written by that area).
//
// The words of the password rule and of the two password fields are the sign-in pages' (`auth.password.*`), so the rule reads the same wherever it is stated.
import type { AreaCopy } from './types.ts'

export const settings = {
  'settings.title': 'Settings',

  // ---- the vendor opt-out ----
  'settings.vendor.title': 'Address verification',
  'settings.vendor.label': 'Never send my addresses to an outside verification service',
  'settings.disclosure.vendor':
    'With this on, we never send your addresses to an outside verification service. Verifying may then leave more addresses undetermined, and it costs less per address. With it off, some addresses may be checked by an outside service, and those cost more.',
  'settings.vendor.scope':
    'A change applies to the verify jobs you create from now on. A job that already exists keeps the setting it was created with.',
  'settings.vendor.savedOn': 'Saved. From now on we will not send your addresses to an outside verification service.',
  'settings.vendor.savedOff': 'Saved. Some of your addresses may now be checked by an outside service, which costs more.',
  'settings.vendor.saving': 'Saving',
  'settings.vendor.failed': 'The setting was not changed.',

  // ---- the account ----
  'settings.account.title': 'Your account',
  'settings.account.email': 'Email address',
  'settings.account.emailUnknown': 'We could not load your email address.',

  // ---- changing the password ----
  'settings.password.title': 'Change your password',
  'settings.password.submit': 'Change the password',
  'settings.password.working': 'Changing the password',
  'settings.password.done': 'Your password was changed.',
  'settings.password.failed': 'We could not change your password. Try again in a moment.',
  'settings.password.sessionEnded': 'Your session ended before the password was changed. Sign in again and try once more.',

  // ---- signing out ----
  'settings.signOut.title': 'Your session',
  'settings.signOut.body': 'End your session on this device. You will have to sign in again to use your account.',
} as const satisfies AreaCopy<'settings'>

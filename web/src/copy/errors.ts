// One customer wording per API error code and per `not_allowed` / `preview_stale` reason (Docs/jobs-api.md, "Errors"), and per error the app makes
// itself (no network, an unreadable reply). Each line says what happened and what to do, in words a customer uses.
// The API's own `message` is never shown (it is a sentence for developers); an unknown code shows `error.unknown` and is logged to the console.
//
// How a line is chosen (copy/index.ts, errorText):
//   code + reason  ->  error.<code>.<reason>        (only for the codes whose reason is a closed list of words: not_allowed, preview_stale)
//   code           ->  error.<code>
//   a line with {needed}, {available}, {shortfall}, {limit} is used only when the error carries the number; otherwise error.<code>.plain
// copy.test.ts holds the three tables below equal to the list of codes in api/errors.ts and to Docs/jobs-api.md.
import type { AreaCopy } from './types.ts'

export const errors = {
  // ---- the app's own errors ----
  'error.unknown': 'Something went wrong. Try again in a moment. If it keeps happening, contact support.',
  'error.network_error': 'We could not reach the service. Check your connection and try again.',
  'error.bad_response': 'We got a reply we could not read. Try again in a moment.',

  // ---- the platform's codes (Docs/jobs-api.md) ----
  'error.unauthenticated': 'Your session has ended. Sign in again to continue.',
  'error.not_found': 'We could not find that. It may have been removed, or the link may be out of date.',
  'error.validation_failed': 'Something you entered was not accepted. Check what you entered and try again.',
  'error.validation_failed.template':
    'Your {part} has a mistake at line {line}, column {column}. Fix it and check it again.',
  'error.bad_request': 'We could not read that request. Reload the page and try again. If it keeps happening, contact support.',
  'error.payload_too_large': 'That is larger than we accept ({limit}). Split it into smaller parts and try again.',
  'error.payload_too_large.plain': 'That is larger than we accept. Split it into smaller parts and try again.',
  'error.too_many_rows': 'This list has more than {limit} addresses. Split it into smaller lists and try again.',
  'error.too_many_rows.plain': 'This list has more addresses than one job can hold. Split it into smaller lists and try again.',
  'error.unsupported_media_type': 'We cannot read that kind of file. Upload a CSV file.',
  'error.nothing_to_do':
    'None of the addresses in this list can be processed: they are invalid, repeated, suppressed, or missing a value your message needs. Review the list and try again.',
  'error.unknown_choice_required':
    'Some addresses in the checked list came back unknown. Choose whether to include or leave them out, then continue.',
  'error.preview_stale':
    'The list, the message or the price changed since you looked at the preview. Review the new preview before you confirm.',
  'error.preview_stale.unknown_choice_changed':
    'You changed what happens to unknown addresses. Review the new preview before you confirm.',
  'error.job_slot_taken':
    'You already have a job of this kind in progress. Wait for it to finish, or cancel it, before you start another.',
  'error.idempotency_key_reused':
    'This confirmation was already used for another job. Reload the page and confirm again.',
  'error.not_previewed': 'This job has not been previewed, or it was confirmed in another window. Reload the page and review it again.',
  'error.infeasible':
    'This campaign cannot run as it is set up. Check your sending accounts and your message, then try again. Nothing was charged.',
  'error.not_allowed': 'You cannot do that to this job in its current state. Reload the page to see where it stands.',
  'error.not_allowed.pause_not_supported': 'A verify job cannot be paused once it is running. You can cancel it instead.',
  'error.not_allowed.cancel_not_supported': 'This job cannot be cancelled once it has started.',
  'error.not_allowed.not_a_draft': 'This job is no longer a draft, so it cannot be changed. Reload the page.',
  'error.not_allowed.already_a_draft': 'This job is already a draft.',
  'error.not_allowed.not_confirmed': 'This job has not been confirmed yet.',
  'error.not_allowed.not_paused': 'This job is not paused, so it cannot be resumed. Reload the page to see where it stands.',
  'error.not_allowed.start_deadline_passed':
    'The time this job was due to start has passed, so it cannot be resumed. Cancel it and create a new job.',
  'error.not_allowed.submitted_job_ends_through_the_engine':
    'This job has already been handed over for processing. Use Cancel on the job page; it will stop as soon as it can.',
  'error.not_allowed.unsubscribe_not_configured':
    'Sending is not available yet because the unsubscribe links are not set up. Nothing was charged. Contact support.',
  'error.not_allowed.opt_out_policy_unavailable':
    'We cannot apply your lookup settings to this job right now. Nothing was charged. Try again in a few minutes.',
  'error.not_allowed.job_status_has_no_preview': 'This job can no longer be previewed. Reload the page to see where it stands.',
  'error.insufficient_credits':
    'This list needs {needed} credits and you have {available}. Add credits or use a shorter list.',
  'error.insufficient_credits.plain': 'You do not have enough credits for this list. Add credits or use a shorter list.',
  'error.no_entitlement':
    'Sending through your own accounts is not switched on for your account. Contact support to have it enabled.',
  'error.suspended': 'Sending through your own accounts is suspended for your account. Contact support.',
  'error.module_unavailable':
    'We cannot reach the service that runs this right now. Nothing was charged. Try again in a few minutes.',
  'error.busy': 'Another change to your credits is in progress. Wait a moment and try again.',
  'error.auth_unavailable':
    'We cannot check your sign-in right now. Your account is fine. Try again in a minute.',
  'error.internal_error':
    'Something went wrong on our side. Try again in a moment. If it keeps happening, contact support and give them the reference.',
  'error.method_not_allowed': 'That action is not available. Reload the page and try again.',

  // ---- the sender accounts of module 3 (Docs/jobs-api.md, "Module 3 accounts"): the codes that module can answer with, and the words of a field's issue ----
  'error.validation_failed.account': 'Some details of the account are not valid. Check the fields marked below and try again.',
  'error.account_duplicate': 'You already added an account with this mail server and username.',
  'error.account_action_unavailable':
    'We cannot add or test accounts right now. If it keeps happening, contact support.',
  'error.account_limit_reached': 'You have reached the number of sending accounts you can have. Remove one you no longer use, or contact support.',
  'error.host_forbidden':
    'We cannot connect to that mail server address. Check the server name, or use the one your email provider gives you.',
  'error.from_gate_ack_required':
    'The address you send from has a warning. Read it, then confirm that you want to add the account anyway.',
  'error.rate_limited': 'Too many tries in a short time. Wait a while and try again.',
  'error.account_outcome_unknown':
    'We could not confirm whether the change was made. Look at your list of accounts before you try again.',
  'error.field.required': 'This is required.',
  'error.field.out_of_range': 'This number is outside the allowed range.',
  'error.field.malformed': 'This is not written correctly.',
  'error.field.not_found': 'We could not find this.',
  'error.field.conflict': 'This conflicts with something you already have.',
  'error.field.forbidden_value': 'This value is not allowed.',
} as const satisfies AreaCopy<'error'>

// Copy of the 'jobs' area: every string a user reads on these screens, keyed 'jobs.<name>'.
// Owned by the screen agent of this area; keep every key under the 'jobs.' prefix (the type enforces it).
// Rules (copy.test.ts holds them): customer wording, no internal word, no snake_case token, no API code.
// Dynamic words from the API (a status, a reason) are written once in common.ts or errors.ts; do not copy them here.
//
// The words of a job's kind (jobs.kind.*) are the ones the dashboard and the jobs screens use; a status and an outcome come from common.ts (copy/words.ts).
// Keys that end in a word of the API (jobs.cost.unit.m1_vendor, jobs.kind.m1) are read with maybeT: a word this file does not know gets a generic line, never the word.
import type { AreaCopy } from './types.ts'

export const jobs = {
  'jobs.title': 'Jobs',
  'jobs.intro': 'Every job you have started, newest first. This page updates by itself while a job is running.',

  // ---- the kind of a job, in the customer's words ----
  'jobs.kind.m1': 'Verification',
  'jobs.kind.m2': 'Managed sending',
  'jobs.kind.m3': 'Your own SMTP',
  'jobs.kind.other': 'Job',
  'jobs.name': '{kind} from {date}',

  // ---- how many addresses, how many credits ----
  'jobs.credits.one': '1 credit',
  'jobs.credits.other': '{count} credits',
  'jobs.count.one': '1 address',
  'jobs.count.other': '{count} addresses',
  'jobs.count.none': 'Not counted yet',

  // ---- the table ----
  'jobs.table.caption': 'Your jobs',
  'jobs.table.job': 'Job',
  'jobs.table.status': 'Status',
  'jobs.table.addresses': 'Addresses',
  'jobs.table.created': 'Created',
  'jobs.table.empty.title': 'You have not started a job yet',
  'jobs.table.empty.body': 'Upload a list of addresses to check them. Your jobs, and their results, are listed here.',

  // ---- the job page ----
  'jobs.detail.title': 'Job details',
  'jobs.detail.back': 'All jobs',
  'jobs.detail.summary': 'Summary',

  'jobs.field.kind': 'Type',
  'jobs.field.created': 'Created',
  'jobs.field.scheduled': 'Scheduled to start',
  'jobs.field.started': 'Started',
  'jobs.field.finished': 'Finished',
  'jobs.field.addresses': 'Addresses in the list',
  'jobs.field.credits': 'Credits set aside',
  'jobs.field.subject': 'Subject',
  'jobs.field.source': 'List taken from',
  'jobs.field.sourceLink': 'An earlier verification job',

  // ---- what is happening now (by status) ----
  'jobs.now.draft': 'This job is a draft. Nothing has been started yet.',
  'jobs.now.previewed': 'This job is ready to confirm. Nothing has been started or charged yet.',
  'jobs.now.confirmed': 'The job is confirmed and will start in a moment.',
  'jobs.now.scheduled': 'The job is scheduled and will start at {date}.',
  'jobs.now.scheduled.noDate': 'The job is scheduled and will start at its set time.',
  'jobs.now.running.m1': 'Your addresses are being checked now. Results appear below as they are ready.',
  'jobs.now.running.send': 'Your messages are being sent now. Results appear below as they are ready.',
  'jobs.now.paused.scheduled': 'The job is paused and has not started. It will not start until you resume it.',
  'jobs.now.paused.running': 'The job is paused. Nothing new is started, and the items already in progress finish. Resume it when you are ready.',
  'jobs.now.cancelling': 'The job is being cancelled. The items already in progress will finish, then the job stops.',
  'jobs.now.completing': 'Almost done. We are recording the last results.',
  'jobs.now.succeeded': 'The job finished. Every address in your list has an outcome below.',
  'jobs.now.cancelled': 'The job was cancelled. What was already done is kept and shown below.',
  'jobs.now.expired': 'The job could not start in time, so it was closed. Nothing was charged.',
  'jobs.now.failed': 'The job could not be completed. Anything we could not process was not charged.',
  'jobs.now.other': 'We cannot tell what this job is doing right now. Reload the page in a moment.',

  // ---- progress ----
  'jobs.progress.label': 'Addresses done',
  'jobs.progress.value': '{done} of {total} addresses done',
  'jobs.progress.done': '{done} addresses done so far',
  'jobs.progress.large': 'This is a large job, so we cannot show how far it has got. Results appear below as they are ready.',

  // ---- continue a job that was not started ----
  'jobs.continue.title': 'This job has not been started',
  'jobs.continue.body': 'Pick up where you left off: review the list, then confirm it.',
  'jobs.continue.link': 'Continue setting up this job',

  // ---- the controls ----
  'jobs.controls.label': 'Job controls',
  'jobs.control.pause': 'Pause the job',
  'jobs.control.resume': 'Resume the job',
  'jobs.control.cancel': 'Cancel the job',
  'jobs.controls.noPause.m1': 'A verification job cannot be paused once it is running. You can cancel it instead.',
  'jobs.controls.noResume.deadline': 'The time this job was due to start has passed, so it cannot be resumed. Cancel it and create a new job.',
  'jobs.controls.noCancel': 'This job cannot be cancelled once it has started.',
  'jobs.controls.pausedLimit': 'A job cannot stay paused for ever: if it is left paused for too long, we cancel it for you.',
  'jobs.toast.paused': 'The job is paused.',
  'jobs.toast.resumedRunning': 'The job is running again.',
  'jobs.toast.resumedScheduled': 'The job will start at its scheduled time.',
  'jobs.toast.cancelled': 'The job is cancelled.',
  'jobs.toast.cancelling': 'Cancel requested. The job stops as soon as the items in progress finish.',

  // ---- the cancel question ----
  'jobs.cancel.title': 'Cancel this job?',
  'jobs.cancel.confirm': 'Cancel the job',
  'jobs.cancel.keep': 'Keep the job',
  'jobs.cancel.notStarted.credits':
    'The job has not started, so nothing has been processed or charged. The credits set aside for it go back to your balance.',
  'jobs.cancel.notStarted.m3': 'The job has not started, so nothing has been sent and nothing is charged.',
  'jobs.cancel.started.m1':
    'Addresses already checked keep their results and are charged. The addresses not yet checked are skipped, and the credits set aside for them go back to your balance. This cannot be undone.',
  'jobs.cancel.started.m2':
    'Messages already sent stay sent and are charged. Messages not yet sent will not be sent, and the credits set aside for them go back to your balance. This cannot be undone.',
  'jobs.cancel.started.m3': 'Messages already sent stay sent. Messages not yet sent will not be sent. This cannot be undone.',
  'jobs.cancel.started.other': 'What was already done stays done. What was not done yet will not be done, and unused credits go back to your balance. This cannot be undone.',
  'jobs.cancel.stopping': 'The job stops as soon as the items already in progress have finished.',

  // ---- results ----
  'jobs.results.title': 'Results',
  'jobs.results.caption': 'Results of this job',
  'jobs.results.address': 'Address',
  'jobs.results.outcome': 'Outcome',
  'jobs.results.cost': 'Cost',
  'jobs.results.details': 'Details',
  'jobs.results.partial': 'The job is still running, so this list is not complete yet. It updates by itself.',
  'jobs.results.empty.title': 'No results yet',
  'jobs.results.empty.live': 'Results appear here as they are ready.',
  'jobs.results.empty.none': 'This job has no results.',
  'jobs.results.filter.label': 'Outcome',
  'jobs.results.filter.hint': 'Narrows down the results shown on this page only.',
  'jobs.results.filter.all': 'All outcomes',
  'jobs.results.filter.empty': 'No result on this page has that outcome. Choose all outcomes, or go to another page.',
  'jobs.results.suppressed': 'This address is on your suppression list.',
  'jobs.results.missingFields': 'Missing a value for: {fields}',
  'jobs.results.excluded': 'Left out: the earlier check found it {verdict}.',

  'jobs.cost.free': 'Free',
  'jobs.cost.charged': 'Charged',
  'jobs.cost.unit.m1_vendor': 'Charged (second lookup)',
  'jobs.cost.notCharged': 'Not charged',
  'jobs.cost.other': 'Not shown',

  // ---- the file ----
  'jobs.csv.button': 'Download CSV',
  'jobs.csv.note': 'One line for every address, with its outcome and whether it was charged.',
  'jobs.csv.noteCleared.send':
    'The file holds the address and the outcome of each row only. The message and the extra columns of your list are no longer kept.',
  'jobs.csv.noteCleared.verify': 'The file holds the address and the outcome of each row only. The extra columns of your list are no longer kept.',
  'jobs.csv.ready': 'Your file is ready. Check your downloads.',
  'jobs.csv.fileName': 'results-{kind}-{date}-{id}.csv',
  'jobs.csv.kind.m1': 'verification',
  'jobs.csv.kind.m2': 'managed-sending',
  'jobs.csv.kind.m3': 'own-smtp',
  'jobs.csv.kind.other': 'job',

  // ---- after the content was cleared ----
  'jobs.cleared.send':
    'The message and the extra columns of your list were removed {after} after this job ended, because we keep them for a limited time only. Your addresses and their results are kept.',
  'jobs.cleared.verify':
    'The extra columns of your list were removed {after} after this job ended, because we keep them for a limited time only. Your addresses and their results are kept.',
  'jobs.days.one': '{n} day',
  'jobs.days.other': '{n} days',
  'jobs.days.some': 'a few days',

  // ---- the history ----
  'jobs.history.title': 'History',
  'jobs.history.empty': 'Nothing has happened to this job yet.',
  'jobs.event.created': 'The job was created.',
  'jobs.event.previewed': 'The list was previewed.',
  'jobs.event.backToDraft': 'The job went back to being a draft.',
  'jobs.event.confirmed': 'The job was confirmed. It starts in a moment.',
  'jobs.event.scheduled': 'The job was scheduled.',
  'jobs.event.resumedScheduled': 'The job was resumed. It starts at its scheduled time.',
  'jobs.event.started': 'The job started.',
  'jobs.event.resumedRunning': 'The job was resumed.',
  'jobs.event.pausedBeforeStart': 'The job was paused before it started.',
  'jobs.event.pausedRunning': 'The job was paused. The items already in progress finish.',
  'jobs.event.cancelling': 'Cancel requested. The job stops when the items in progress finish.',
  'jobs.event.completing': 'Every item is done. The last results are being recorded.',
  'jobs.event.succeeded': 'The job finished.',
  'jobs.event.discarded': 'The job was discarded.',
  'jobs.event.cancelledBeforeStart': 'The job was cancelled before it started.',
  'jobs.event.cancelledAfterStart': 'The job was cancelled. What was already done is kept.',
  'jobs.event.pauseLimitCancelling': 'The job was paused for too long, so it is being cancelled.',
  'jobs.event.pauseLimitCancelled': 'The job was paused for too long, so it was cancelled.',
  'jobs.event.expired': 'The job could not start in time, so it was closed. Nothing was charged.',
  'jobs.event.failed': 'The job could not be completed.',
  'jobs.event.byTeam': 'Our team moved the job to: {status}.',
  'jobs.event.transition.other': 'The job changed state.',
  'jobs.event.stale': 'The job has been waiting longer than usual. It is still in line and keeps trying.',
  'jobs.event.flag': 'Our team was alerted to something on this job.',
  'jobs.event.note.paused': 'Your pause has taken effect. Nothing new is started until you resume.',
  'jobs.event.note.resumed': 'Work on the job carries on.',
  'jobs.event.note.other': 'An update was recorded on the job.',
  'jobs.event.other': 'Something happened to the job.',
} as const satisfies AreaCopy<'jobs'>

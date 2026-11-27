// Which of Pause, Resume and Cancel a job offers, and what is said when a control people may expect is not there.
//
// The rules are the API's (Docs/jobs-api.md, "The routes" and "Pause, resume and cancel of a running job"), written here so the screen never offers a control the
// platform will refuse for a reason it already knows:
//
//   draft, previewed        none: the job is continued on the New job screen
//   confirmed               Cancel (nothing was submitted yet; there is no pause before the start)
//   scheduled               Pause (any kind), Cancel
//   running                 Pause only for a job that sends (a verification job has no pause: pause_not_supported), Cancel
//   paused                  Resume (not when it paused before its start and that start's deadline has passed: start_deadline_passed), Cancel
//   cancelling, completing, finished (succeeded, cancelled, expired, failed)   none
//
// A refusal the platform made anyway (`refused`: the `not_allowed` reasons the screen has already met for this status) removes the control too: cancel_not_supported
// is reserved by the API and returned only if a module's capability row says it cannot cancel.
import type { Job } from '../../api/types.ts'
import type { CopyKey } from '../../copy/index.ts'

export type Refusal = 'pause_not_supported' | 'cancel_not_supported' | 'start_deadline_passed'

/** The `not_allowed` reasons that remove a control for good (for this status). Any other reason is shown and changes nothing. */
export function refusalOf(reason: string | undefined): Refusal | null {
  return reason === 'pause_not_supported' || reason === 'cancel_not_supported' || reason === 'start_deadline_passed' ? reason : null
}

export type CancelKind = 'notStarted' | 'started'

export interface JobControls {
  pause: boolean
  resume: boolean
  cancel: boolean
  /** what the cancel question tells: nothing was submitted yet, or the job has started */
  cancelKind: CancelKind
  /** calm sentences for a control that is not there but may be expected */
  notes: CopyKey[]
}

type ControlJob = Pick<Job, 'module' | 'status' | 'paused_from' | 'start_deadline'>

export function controlsFor(job: ControlJob, now: number = Date.now(), refused: ReadonlySet<Refusal> = new Set()): JobControls {
  const notes: CopyKey[] = []
  let pause = false
  let resume = false
  let cancel = false
  // A job that paused before its start has submitted nothing; any other pause holds a started job (when in doubt, the cautious sentence).
  const cancelKind: CancelKind = job.status === 'paused' ? (job.paused_from === 'scheduled' ? 'notStarted' : 'started') : job.status === 'running' ? 'started' : 'notStarted'

  switch (job.status) {
    case 'confirmed':
      cancel = true
      break
    case 'scheduled':
      pause = true
      cancel = true
      break
    case 'running':
      cancel = true
      if (job.module === 'm1') {
        notes.push('jobs.controls.noPause.m1')
      } else {
        pause = true
      }
      break
    case 'paused': {
      cancel = true
      resume = true
      const deadline = job.start_deadline === null ? Number.NaN : new Date(job.start_deadline).getTime()
      if (job.paused_from === 'scheduled' && !Number.isNaN(deadline) && deadline <= now) {
        resume = false
        notes.push('jobs.controls.noResume.deadline')
      }
      notes.push('jobs.controls.pausedLimit')
      break
    }
    default:
      break
  }

  if (refused.has('pause_not_supported') && pause) {
    pause = false
    if (job.module === 'm1') notes.push('jobs.controls.noPause.m1')
  }
  if (refused.has('cancel_not_supported') && cancel) {
    cancel = false
    notes.push('jobs.controls.noCancel')
  }
  if (refused.has('start_deadline_passed') && resume) {
    resume = false
    notes.push('jobs.controls.noResume.deadline')
  }

  return { pause, resume, cancel, cancelKind, notes: [...new Set(notes)] }
}

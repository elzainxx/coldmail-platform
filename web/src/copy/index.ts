// THE copy catalogue: every string a user reads. Screens never write a sentence in a component; they call t('area.key').
//
//   t(key, vars?)        typed: a key that is not in a catalogue file is a compile error
//   errorText(err)       the one line for an ApiError (see errors.ts for how it is chosen); an unknown code shows ONE generic line and is logged
//   templateProblem(err) the position of a mistake in a user's own template (the one server text a screen may show: see decision 5 of the spec)
//
// Files: common.ts, auth.ts, errors.ts (all mine) and one per area (dashboard, jobs, newjob, accounts, suppressions, settings), each owned by its screen agent.
import type { ApiError } from '../api/errors.ts'
import { CLOSED_REASON_CODES } from '../api/errors.ts'
import { accounts } from './accounts.ts'
import { auth } from './auth.ts'
import { common } from './common.ts'
import { dashboard } from './dashboard.ts'
import { errors } from './errors.ts'
import { jobs } from './jobs.ts'
import { newjob } from './newjob.ts'
import { settings } from './settings.ts'
import { suppressions } from './suppressions.ts'

export const catalogue = {
  ...common,
  ...auth,
  ...errors,
  ...dashboard,
  ...jobs,
  ...newjob,
  ...accounts,
  ...suppressions,
  ...settings,
} as const

export type CopyKey = keyof typeof catalogue
export type CopyVars = Readonly<Record<string, string | number>>

const lookup: Readonly<Record<string, string>> = catalogue

const PLACEHOLDER = /\{([A-Za-z][A-Za-z0-9]*)\}/g

/** The text with every {name} replaced, or null when a name has no value. */
export function fill(text: string, vars?: CopyVars): string | null {
  let complete = true
  const out = text.replace(PLACEHOLDER, (_whole, name: string) => {
    const v = vars?.[name]
    if (v === undefined) {
      complete = false
      return ''
    }
    return String(v)
  })
  return complete ? out : null
}

/** The sentence for a key, with {name} filled from vars. A name without a value is left empty and logged (a mistake in the caller). */
export function t(key: CopyKey, vars?: CopyVars): string {
  const text = lookup[key] ?? ''
  const out = fill(text, vars)
  if (out !== null) return out
  console.warn(`copy: "${key}" was used without all of its values`)
  return text.replace(PLACEHOLDER, (_whole, name: string) => String(vars?.[name] ?? ''))
}

/** Like t for a key built at run time (a word from the API): null when there is no such line. Never returns the word itself. */
export function maybeT(key: string, vars?: CopyVars): string | null {
  const text = lookup[key]
  return text === undefined ? null : fill(text, vars)
}

// ---- errors ----

const nf = new Intl.NumberFormat('en-US')

function formatBytes(n: number): string {
  if (n >= 1024 * 1024) return `${nf.format(Math.round(n / (1024 * 1024)))} MB`
  if (n >= 1024) return `${nf.format(Math.round(n / 1024))} KB`
  return `${nf.format(n)} bytes`
}

function numberAt(details: Record<string, unknown> | undefined, name: string): number | undefined {
  const v = details?.[name]
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined
}

const PARTS = ['subject', 'html', 'text'] as const
type Part = (typeof PARTS)[number]

export interface TemplateProblem {
  part: Part
  line: number
  column: number
  /** The template package's own sentence about the user's own template (it quotes at most one character of it). */
  message: string
}

/** The position of a mistake in a message template, or null when the error is not one. */
export function templateProblem(err: ApiError): TemplateProblem | null {
  if (err.code !== 'validation_failed') return null
  const d = err.details
  const part = d?.['part']
  const line = numberAt(d, 'line')
  const column = numberAt(d, 'column')
  if (!PARTS.includes(part as Part) || line === undefined || column === undefined) return null
  const message = d?.['message']
  return { part: part as Part, line, column, message: typeof message === 'string' ? message : '' }
}

/** The one line a user reads for an ApiError. Never the server's message, never a code. */
export function errorText(err: ApiError): string {
  const problem = templateProblem(err)
  if (problem) {
    return t('error.validation_failed.template', {
      part: t(`common.part.${problem.part}`),
      line: problem.line,
      column: problem.column,
    })
  }

  // A module 3 account refused field by field (details.fields is a list of "field:issue" words): fieldIssuesOf in words.ts gives the lines to put by each field.
  if (err.code === 'validation_failed' && Array.isArray(err.details?.['fields'])) return t('error.validation_failed.account')

  const vars: Record<string, string | number> = {}
  const needed = numberAt(err.details, 'needed')
  const available = numberAt(err.details, 'available')
  const shortfall = numberAt(err.details, 'shortfall')
  const limit = numberAt(err.details, 'limit')
  const limitBytes = numberAt(err.details, 'limit_bytes')
  if (needed !== undefined) vars['needed'] = nf.format(needed)
  if (available !== undefined) vars['available'] = nf.format(available)
  if (shortfall !== undefined) vars['shortfall'] = nf.format(shortfall)
  if (limit !== undefined) vars['limit'] = nf.format(limit)
  if (limitBytes !== undefined) vars['limit'] = formatBytes(limitBytes)

  const candidates: string[] = []
  if (err.reason !== undefined && (CLOSED_REASON_CODES as readonly string[]).includes(err.code)) {
    candidates.push(`error.${err.code}.${err.reason}`)
  }
  candidates.push(`error.${err.code}`)

  for (const [i, key] of candidates.entries()) {
    const line = maybeT(key, vars) ?? maybeT(`${key}.plain`, vars)
    if (line !== null) {
      // The code has a line but its reason has none: the platform grew a reason. The user still gets the code's line.
      if (i > 0) console.warn(`copy: no wording for the reason "${err.reason}" of the API error code "${err.code}"`)
      return line
    }
  }

  // A code this app has no wording for (the platform grew a code): one generic line for the user, the code for the developer.
  console.warn(`copy: no wording for the API error code "${err.code}"${err.reason === undefined ? '' : ` (reason "${err.reason}")`}`)
  return t('error.unknown')
}

/** "Reference req_..." for the support request that goes with an error, or null when the error has none. */
export function errorReference(err: ApiError): string | null {
  return err.requestId === undefined ? null : t('common.reference', { id: err.requestId })
}

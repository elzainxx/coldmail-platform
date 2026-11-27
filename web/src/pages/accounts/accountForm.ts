// What the add-account form sends, read from the form at the moment of the submit (never held in state: the password is in the input, then in the one request, and
// the input is cleared). Pure, so a test can feed it a form.
import type { M3NewAccount } from '../../api/types.ts'
import { fieldIssueText } from '../../copy/words.ts'

/** The members of a new account, which are also the names of the form's inputs (and of the columns of a bulk file). */
export const ACCOUNT_FIELDS = ['smtp_host', 'smtp_port', 'smtp_username', 'smtp_secret', 'from_address', 'from_name', 'daily_limit'] as const
export type AccountField = (typeof ACCOUNT_FIELDS)[number]
export type FieldErrors = Partial<Record<AccountField, string>>

export const isAccountField = (name: string): name is AccountField => (ACCOUNT_FIELDS as readonly string[]).includes(name)

/** The API takes at most this many bytes in a text member; characters are a fair bound for the form. */
const MAX_TEXT = 1024
const MAX_PORT = 65535

export interface ParsedAccount {
  /** null when something is wrong: `errors` says what, by field, in words */
  account: M3NewAccount | null
  errors: FieldErrors
}

/** The first header line of a bulk add: the columns of the API in the order the screen states them. */
export const BULK_HEADER = 'smtp_host,smtp_port,smtp_username,smtp_secret,from_address,from_name,daily_limit'

export function parseAccountForm(data: FormData, acknowledge: boolean): ParsedAccount {
  const text = (name: AccountField): string => {
    const v = data.get(name)
    return typeof v === 'string' ? v : ''
  }
  const errors: FieldErrors = {}
  const must = (name: AccountField, value: string): void => {
    if (value.trim() === '') errors[name] = fieldIssueText('required')
    else if (value.length > MAX_TEXT) errors[name] = fieldIssueText('malformed')
  }

  const host = text('smtp_host').trim()
  must('smtp_host', host)
  if (errors.smtp_host === undefined && /\s/.test(host)) errors.smtp_host = fieldIssueText('malformed')

  const username = text('smtp_username').trim()
  must('smtp_username', username)

  const secret = text('smtp_secret') // never trimmed: a password may begin or end with a space
  must('smtp_secret', secret)

  const from = text('from_address').trim()
  must('from_address', from)
  if (errors.from_address === undefined && !/^[^@\s]+@[^@\s]+$/.test(from)) errors.from_address = fieldIssueText('malformed')

  const name = text('from_name').trim()
  if (name.length > MAX_TEXT) errors.from_name = fieldIssueText('malformed')

  const wholeNumber = (field: AccountField, raw: string, min: number, max: number): number | undefined => {
    if (raw === '') return undefined
    if (!/^\d{1,9}$/.test(raw)) {
      errors[field] = fieldIssueText('malformed')
      return undefined
    }
    const n = Number(raw)
    if (n < min || n > max) {
      errors[field] = fieldIssueText('out_of_range')
      return undefined
    }
    return n
  }
  const port = wholeNumber('smtp_port', text('smtp_port').trim(), 1, MAX_PORT)
  const daily = wholeNumber('daily_limit', text('daily_limit').trim(), 1, 1_000_000_000)

  if (Object.keys(errors).length > 0) return { account: null, errors }

  const account: M3NewAccount = { smtp_host: host, smtp_username: username, smtp_secret: secret, from_address: from }
  if (port !== undefined) account.smtp_port = port
  if (name !== '') account.from_name = name
  if (daily !== undefined) account.daily_limit = daily
  if (acknowledge) account.acknowledge_from_gate = true
  return { account, errors }
}

// A failed call in words, for this area's screens: the one line of the catalogue and the reference to quote to support. Never the server's message, never a code.
import { isApiError } from '../../api/errors.ts'
import { errorReference, errorText, t } from '../../copy/index.ts'

export interface Failure {
  text: string
  reference: string | null
}

export function failureOf(err: unknown): Failure {
  // A thrown value that is not an ApiError is a mistake of the app's own: the user gets the generic line.
  if (!isApiError(err)) return { text: t('error.unknown'), reference: null }
  return { text: errorText(err), reference: errorReference(err) }
}

export function FailureNote({ failure }: { failure: Failure }) {
  return (
    <div role="alert" className="rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
      <p>{failure.text}</p>
      {failure.reference !== null && <p className="mt-1 text-xs">{failure.reference}</p>}
    </div>
  )
}

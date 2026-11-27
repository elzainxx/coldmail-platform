import type { Failure } from './words.ts'

/** A failed call in words: the line, the wait when a rate limit gave one, and the reference to quote to support. An alert, so a screen reader says it at once. */
export function ErrorNote({ failure }: { failure: Failure }) {
  return (
    <div role="alert" className="rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
      <p>{failure.text}</p>
      {failure.wait !== null && <p>{failure.wait}</p>}
      {failure.reference !== null && <p className="mt-1 text-xs">{failure.reference}</p>}
    </div>
  )
}

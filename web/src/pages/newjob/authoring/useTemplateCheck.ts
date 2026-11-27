// The live check of a message while it is written: 400 ms after the last keystroke the three parts go to POST /v1/templates/check, and an answer that a
// newer keystroke has made old is thrown away (the request itself is aborted, so the platform stops too).
//
//   pending   a check is waiting for the typing to stop, or is out
//   result    the last answer: the fields the message uses, the mistakes (with part, line, column and the grammar's own message), or "could not check"
//             (kept while the next check is pending, so the marks do not flicker while the user types)
import { useEffect, useState } from 'react'
import { templatesApi } from '../../../api/endpoints.ts'
import type { ApiError } from '../../../api/errors.ts'
import type { TemplateCheckError, TemplateCheckRequest, TemplateField } from '../../../api/types.ts'
import { isAbort, toApiError } from '../errors.ts'

export const CHECK_DELAY_MS = 400

export type CheckResult =
  | { kind: 'ok'; fields: TemplateField[] }
  | { kind: 'errors'; errors: TemplateCheckError[] }
  | { kind: 'failed'; error: ApiError }

export interface CheckState {
  pending: boolean
  result: CheckResult | null
}

export function useTemplateCheck({ subject, html, text }: TemplateCheckRequest, delayMs: number = CHECK_DELAY_MS): CheckState {
  const blank = subject.trim() === '' && html.trim() === '' && text.trim() === ''
  const [state, setState] = useState<CheckState>({ pending: !blank, result: null })

  useEffect(() => {
    if (blank) {
      setState({ pending: false, result: null })
      return undefined
    }
    setState((s) => (s.pending ? s : { ...s, pending: true }))
    const controller = new AbortController()
    const timer = setTimeout(() => {
      templatesApi.check({ subject, html, text }, { signal: controller.signal }).then(
        (answer) => {
          if (controller.signal.aborted) return
          setState({ pending: false, result: answer.ok ? { kind: 'ok', fields: answer.fields } : { kind: 'errors', errors: answer.errors } })
        },
        (err: unknown) => {
          if (controller.signal.aborted || isAbort(err)) return
          setState({ pending: false, result: { kind: 'failed', error: toApiError(err) } })
        },
      )
    }, delayMs)
    return () => {
      clearTimeout(timer)
      controller.abort()
    }
  }, [subject, html, text, blank, delayMs])

  return state
}

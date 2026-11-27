// The mistake in one part of a message, under its field: where it is (line and column), the grammar's own sentence about it (the one text of the platform's a
// screen may show: it describes the user's own template, and quotes at most one character of it), and a marker under the place in the line.
import type { TemplateCheckError } from '../../../api/types.ts'
import { t } from '../../../copy/index.ts'
import { errorExcerpt } from './text.ts'

/** The one line a mistake reads as: "Line 2, column 5: unexpected ...". */
export function problemLine(e: TemplateCheckError): string {
  return t('newjob.authoring.errorAt', { line: e.line, column: e.column, message: e.message })
}

/** The line of the template the mistake is on, with a caret under the column. Decoration: the text above already says where. */
export function ErrorMarker({ source, error }: { source: string; error: TemplateCheckError }) {
  const excerpt = errorExcerpt(source, error.line, error.column)
  if (excerpt === null) return null
  return (
    <div aria-hidden="true" className="mt-1 overflow-x-auto rounded-md border border-danger bg-sunken px-2 py-1 font-mono text-xs text-fg">
      <div className="whitespace-pre">{excerpt.text}</div>
      <div className="whitespace-pre font-bold text-danger">{`${' '.repeat(excerpt.caret)}^`}</div>
    </div>
  )
}

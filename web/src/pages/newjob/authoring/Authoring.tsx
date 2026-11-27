// The authoring screen: subject, an HTML box with the tags a person reaches for, an optional plain-text part, a reply-to, a palette that inserts the list's
// columns as merge fields, a live (sandboxed) view of the HTML, the help for the grammar, and the platform's own judgement of the message under each field
// as it is written (useTemplateCheck.ts): where the mistake is, the grammar's sentence about it, and a marker under the place.
//
// No third-party editor (decision 3 of the spec). The HTML box is a text area; the screen's own view of it is rendered ONLY by the sandboxed frame.
import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { TemplateCheckError } from '../../../api/types.ts'
import { errorReference, errorText, t } from '../../../copy/index.ts'
import { Card, FormField, TextArea, TextInput } from '../../../ui/index.ts'
import type { MergeColumn } from '../csv/columns.ts'
import { SandboxedFrame } from '../SandboxedFrame.tsx'
import { GrammarHelp } from './GrammarHelp.tsx'
import { HtmlToolbar } from './HtmlToolbar.tsx'
import { problemOf, replyToValid, type Message, type Part } from './message.ts'
import { MergePalette } from './MergePalette.tsx'
import { ErrorMarker, problemLine } from './PartError.tsx'
import { applyInsert, displayHtml, type Snippet } from './text.ts'
import { CHECK_DELAY_MS, type CheckState } from './useTemplateCheck.ts'

function useDebounced<T>(value: T, ms: number): T {
  const [shown, setShown] = useState(value)
  useEffect(() => {
    const timer = setTimeout(() => setShown(value), ms)
    return () => clearTimeout(timer)
  }, [value, ms])
  return shown
}

const PART_WORDS = { subject: 'common.part.subject', html: 'common.part.html', text: 'common.part.text' } as const

export interface AuthoringProps {
  message: Message
  onChange: (patch: Partial<Message>) => void
  /** the columns of the list other than the address; empty for a send to a finished verification */
  columns: readonly MergeColumn[]
  /** where the fields of a CSV list can be named: false for a list that has no columns (a finished verification) */
  hasColumns: boolean
  check: CheckState
  /** a mistake the platform found when it saved the message (the live check should have caught it first) */
  serverProblem: TemplateCheckError | null
  /** the message is too long to travel with a file */
  tooLong: boolean
}

export function Authoring({ message, onChange, columns, hasColumns, check, serverProblem, tooLong }: AuthoringProps) {
  const [active, setActive] = useState<Part>('html')
  const [replyTouched, setReplyTouched] = useState(false)
  const boxes = useRef<Record<Part, HTMLDivElement | null>>({ subject: null, html: null, text: null })
  const pending = useRef<{ part: Part; start: number; end: number } | null>(null)

  // After an insert the text is the new one: put the selection where the insert left it and give the box the focus back.
  useLayoutEffect(() => {
    const p = pending.current
    if (p === null) return
    pending.current = null
    const el = boxes.current[p.part]?.querySelector<HTMLInputElement | HTMLTextAreaElement>('input, textarea')
    if (el) {
      el.focus()
      el.setSelectionRange(p.start, p.end)
    }
  })

  const insert = (part: Part, snippet: Snippet) => {
    const el = boxes.current[part]?.querySelector<HTMLInputElement | HTMLTextAreaElement>('input, textarea')
    const value = message[part]
    const edit = applyInsert(value, el?.selectionStart ?? value.length, el?.selectionEnd ?? value.length, snippet)
    pending.current = { part, start: edit.start, end: edit.end }
    onChange({ [part]: edit.value } as Partial<Message>)
  }

  const liveErrors = check.result?.kind === 'errors' ? check.result.errors : []
  const problem = (part: Part): TemplateCheckError | null => problemOf(liveErrors, part) ?? (serverProblem?.part === part ? serverProblem : null)

  const shownHtml = useDebounced(message.html, CHECK_DELAY_MS)
  const replyError = replyTouched && !replyToValid(message.replyTo) ? t('newjob.authoring.replyToInvalid') : null

  const known = new Set(columns.map((c) => c.name))
  const unmet =
    check.result?.kind === 'ok' ? check.result.fields.filter((f) => !f.has_fallback && !known.has(f.name)) : []

  const field = (part: Part) => {
    const p = problem(part)
    return { error: p === null ? null : problemLine(p), marker: p === null ? null : <ErrorMarker source={message[part]} error={p} /> }
  }
  const subject = field('subject')
  const html = field('html')
  const text = field('text')

  return (
    <div className="flex flex-col gap-4">
      <div
        ref={(el) => {
          boxes.current.subject = el
        }}
      >
        <FormField label={t('newjob.authoring.subject')} required error={subject.error}>
          <TextInput value={message.subject} onChange={(e) => onChange({ subject: e.target.value })} onFocus={() => setActive('subject')} autoComplete="off" />
        </FormField>
      </div>
      {subject.marker}

      <FormField label={t('newjob.authoring.replyTo')} hint={t('newjob.authoring.replyToHint')} optional error={replyError}>
        <TextInput
          type="email"
          value={message.replyTo}
          onChange={(e) => onChange({ replyTo: e.target.value })}
          onBlur={() => setReplyTouched(true)}
          autoComplete="off"
          spellCheck={false}
        />
      </FormField>

      <MergePalette columns={columns} target={t(PART_WORDS[active])} onInsert={(snippet) => insert(active, { before: snippet, replace: true })} />

      <div className="flex flex-col gap-2">
        <HtmlToolbar
          onInsert={(snippet) => {
            setActive('html')
            insert('html', snippet)
          }}
        />
        <div
          ref={(el) => {
            boxes.current.html = el
          }}
        >
          <FormField label={t('newjob.authoring.html')} hint={t('newjob.authoring.htmlHint')} required error={html.error}>
            <TextArea mono rows={14} spellCheck={false} value={message.html} onChange={(e) => onChange({ html: e.target.value })} onFocus={() => setActive('html')} />
          </FormField>
        </div>
        {html.marker}
      </div>

      <Card title={t('newjob.authoring.previewTitle')}>
        <p className="mb-2 text-sm text-fg-muted">{t('newjob.authoring.previewNote')}</p>
        {shownHtml.trim() === '' ? (
          <p className="text-sm text-fg-muted">{t('newjob.authoring.previewEmpty')}</p>
        ) : (
          <SandboxedFrame html={displayHtml(shownHtml)} title={t('newjob.authoring.previewFrame')} />
        )}
      </Card>

      <div
        ref={(el) => {
          boxes.current.text = el
        }}
      >
        <FormField label={t('newjob.authoring.text')} hint={t('newjob.authoring.textHint')} optional error={text.error}>
          <TextArea mono rows={6} spellCheck={false} value={message.text} onChange={(e) => onChange({ text: e.target.value })} onFocus={() => setActive('text')} />
        </FormField>
      </div>
      {text.marker}

      <div role="status" aria-live="polite" className="flex flex-col gap-1 text-sm">
        {check.pending ? (
          <p className="text-fg-muted">{t('newjob.authoring.checking')}</p>
        ) : check.result?.kind === 'ok' ? (
          <>
            <p className="text-fg">{t('newjob.authoring.ok')}</p>
            {check.result.fields.length > 0 && (
              <p className="text-fg-muted">
                {t('newjob.authoring.fieldsUsed')}{' '}
                {check.result.fields.map((f, i) => (
                  <span key={f.name}>
                    {i > 0 && ', '}
                    <span className="font-mono text-fg">{f.name}</span> ({f.has_fallback ? t('newjob.authoring.hasFallback') : t('newjob.authoring.noFallback')})
                  </span>
                ))}
              </p>
            )}
          </>
        ) : check.result?.kind === 'failed' ? (
          <p className="text-fg-muted">
            {t('newjob.authoring.checkFailed')} {errorText(check.result.error)} {errorReference(check.result.error) ?? ''}
          </p>
        ) : null}
        {unmet.length > 0 && (
          <p className="text-fg">
            {hasColumns ? t('newjob.authoring.unmetColumn') : t('newjob.authoring.unmetNoColumns')}{' '}
            <span className="font-mono">{unmet.map((f) => f.name).join(', ')}</span>
          </p>
        )}
        {tooLong && <p className="font-medium text-danger">{t('newjob.authoring.tooLong')}</p>}
      </div>

      <GrammarHelp />
    </div>
  )
}

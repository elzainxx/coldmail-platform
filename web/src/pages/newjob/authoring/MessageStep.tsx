// Step 3 of a send: the message. The authoring screen plus the check that lets the user go on: a subject, a body, no mistake the platform found, and no
// check still on its way. A check that could not be made does not block (the platform judges the message again when the draft is made).
import type { TemplateCheckError } from '../../../api/types.ts'
import { t } from '../../../copy/index.ts'
import type { MergeColumn } from '../csv/columns.ts'
import { StepNav } from '../Steps.tsx'
import { Authoring } from './Authoring.tsx'
import { messageComplete, replyToValid, type Message } from './message.ts'
import { useTemplateCheck } from './useTemplateCheck.ts'

export interface MessageStepProps {
  message: Message
  onChange: (patch: Partial<Message>) => void
  columns: readonly MergeColumn[]
  hasColumns: boolean
  paletteNote?: string
  serverProblem: TemplateCheckError | null
  tooLong: boolean
  onBack: () => void
  onNext: () => void
}

export function MessageStep({ message, onChange, columns, hasColumns, paletteNote, serverProblem, tooLong, onBack, onNext }: MessageStepProps) {
  const check = useTemplateCheck({ subject: message.subject, html: message.html, text: message.text })

  const hint = !messageComplete(message)
    ? t('newjob.message.needSubjectAndBody')
    : !replyToValid(message.replyTo)
      ? t('newjob.message.fixMistakes')
      : check.pending
        ? t('newjob.authoring.checking')
        : check.result?.kind === 'errors' || serverProblem !== null
          ? t('newjob.message.fixMistakes')
          : tooLong
            ? t('newjob.authoring.tooLong')
            : null
  const ready = hint === null

  return (
    <>
      <Authoring message={message} onChange={onChange} columns={columns} hasColumns={hasColumns} paletteNote={paletteNote} check={check} serverProblem={serverProblem} tooLong={tooLong} />
      <StepNav onBack={onBack} onNext={onNext} nextLabel={t('newjob.message.next')} nextDisabled={!ready} nextHint={hint} />
    </>
  )
}

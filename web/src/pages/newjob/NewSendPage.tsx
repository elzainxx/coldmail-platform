// New send job (/new/send), in four steps: who sends (Managed sending or Your own SMTP), the list (a CSV file, or the results of a finished verification), the
// message (the authoring screen) and the preview with the confirm. A draft the user left can be continued with `?job=<id>`, which goes straight to the preview.
//
// Nothing is made on the platform until the user goes from the message to the preview: the draft is created then (the platform freezes a job's message when it is
// saved) and previewed. Going back from the preview lets the draft go (it holds nothing) so the message can be changed and a new draft made.
import { useEffect, useState, type ReactNode } from 'react'
import type { Job, TemplateCheckError, UnknownChoice } from '../../api/types.ts'
import { t, templateProblem } from '../../copy/index.ts'
import { navigate, useRoute } from '../../router.tsx'
import { Card, PageHeader } from '../../ui/index.ts'
import { EMPTY_MESSAGE, type Message } from './authoring/message.ts'
import { MessageStep } from './authoring/MessageStep.tsx'
import { chainedCreate, csvCreate, csvQuery, fitsInQuery } from './createRequest.ts'
import { mergeColumns } from './csv/columns.ts'
import { fieldColumns, mergeFieldsOf } from './mergeFields.ts'
import { ListStep, type ListKind } from './ListStep.tsx'
import { ReviewSection } from './ReviewSection.tsx'
import { SenderStep, type Sender } from './SenderStep.tsx'
import { StepFrame, StepNav, Stepper, type StepInfo } from './Steps.tsx'
import { useCsvSource } from './useCsvSource.ts'
import { useDraft } from './useDraft.ts'
import { useAccountsFact, useBalances } from './useSenderFacts.ts'

type StepId = 'sender' | 'list' | 'message' | 'preview'
const STEP_IDS: readonly StepId[] = ['sender', 'list', 'message', 'preview']

function stepLabel(id: StepId): string {
  return t(
    id === 'sender' ? 'newjob.step.sender' : id === 'list' ? 'newjob.step.list' : id === 'message' ? 'newjob.step.message' : 'newjob.step.preview',
  )
}

export function NewSendPage() {
  const { query } = useRoute()
  const jobParam = query.get('job')
  const continuing = jobParam !== null && jobParam !== ''

  const [step, setStep] = useState<StepId>('sender')
  const [moved, setMoved] = useState(false)
  const [sender, setSender] = useState<Sender | null>(null)
  const [listKind, setListKind] = useState<ListKind>('csv')
  const csv = useCsvSource()
  const [verification, setVerification] = useState<Job | null>(null)
  const [unknownChoice, setUnknownChoice] = useState<UnknownChoice | null>(null)
  const [message, setMessage] = useState<Message>(EMPTY_MESSAGE)
  const [serverProblem, setServerProblem] = useState<TemplateCheckError | null>(null)
  const [tooLong, setTooLong] = useState(false)

  const balances = useBalances()
  const accounts = useAccountsFact(sender === 'm3')
  const draft = useDraft()
  const { load, reset } = draft
  const draftError = draft.error

  useEffect(() => {
    if (jobParam !== null && jobParam !== '') void load(jobParam, (j) => j.module !== 'm1')
  }, [jobParam, load])

  // The platform judged the message differently from the live check (it should not happen): show its mistake at the field, back on the message step.
  useEffect(() => {
    const problem = draftError === null ? null : templateProblem(draftError)
    if (problem !== null) {
      setServerProblem(problem)
      setStep('message')
      setMoved(true)
      reset()
    }
  }, [draftError, reset])

  const go = (next: StepId) => {
    setStep(next)
    setMoved(true)
  }

  const edit = (patch: Partial<Message>) => {
    setMessage((m) => ({ ...m, ...patch }))
    setServerProblem(null)
    setTooLong(false)
  }

  const listReady = listKind === 'csv' ? csv.ready : verification !== null && unknownChoice !== null
  const listHint =
    listKind === 'csv'
      ? csv.file === null
        ? t('newjob.verify.needFile')
        : csv.reading || csv.head === null
          ? t('newjob.verify.waitRead')
          : csv.chosen === null
            ? t('newjob.verify.needColumn')
            : t('newjob.verify.fixProblems')
      : verification === null
        ? t('newjob.list.needVerification')
        : t('newjob.list.needChoice')

  // the fields a message may name: the other columns of the file, or the fields of the finished verification the user chose (its `merge_fields`)
  const columns = listKind === 'csv' ? (csv.head !== null ? mergeColumns(csv.head.header, csv.chosen) : []) : fieldColumns(mergeFieldsOf(verification))

  const toPreview = () => {
    if (sender === null) return
    if (listKind === 'csv') {
      const { file, head, chosen } = csv
      if (file === null || head === null || chosen === null) return
      const query = csvQuery(sender, head.header[chosen] ?? '', message)
      if (!fitsInQuery(query)) {
        setTooLong(true)
        return
      }
      go('preview')
      void draft.start(csvCreate(file, query))
      return
    }
    if (verification === null) return
    go('preview')
    void draft.start(chainedCreate(sender, verification.id, message, unknownChoice))
  }

  /** Back from the preview to the message: the draft is let go (its message cannot be changed), and a new one is made on the way forward. */
  const editMessage = () => {
    void draft.discard()
    go('message')
  }

  const restartContinued = () => {
    void draft.discard()
    navigate('/new/send', { replace: true })
  }

  if (continuing) {
    const content = draft.job?.content
    return (
      <section className="flex flex-col gap-4">
        <PageHeader title={t('newjob.send.title')} description={t('newjob.send.introContinue')} />
        {content !== undefined && draft.result === null && (
          <Card title={t('newjob.send.yourMessage')}>
            <dl className="m-0 flex flex-col gap-2">
              <div>
                <dt className="text-sm font-medium text-fg-muted">{t('newjob.samples.subject')}</dt>
                <dd className="m-0 text-base text-fg">{content.subject}</dd>
              </div>
              {content.reply_to !== '' && (
                <div>
                  <dt className="text-sm font-medium text-fg-muted">{t('newjob.authoring.replyTo')}</dt>
                  <dd className="m-0 text-base text-fg">{content.reply_to}</dd>
                </div>
              )}
            </dl>
          </Card>
        )}
        <ReviewSection draft={draft} expect="send" balances={balances} settingsOptedOut={null} onRestart={restartContinued} restartLabel={t('newjob.send.startOver')} />
      </section>
    )
  }

  const index = STEP_IDS.indexOf(step)
  const steps: StepInfo[] = STEP_IDS.map((id) => ({ id, label: stepLabel(id) }))
  const frame = (children: ReactNode) => (
    <StepFrame key={step} index={index + 1} total={STEP_IDS.length} title={stepLabel(step)} focusOnShow={moved}>
      {children}
    </StepFrame>
  )

  return (
    <section className="flex flex-col gap-4">
      <PageHeader title={t('newjob.send.title')} description={t('newjob.send.intro')} />
      <Stepper steps={steps} current={index} />

      {step === 'sender' &&
        frame(
          <>
            <SenderStep sender={sender} onSender={setSender} balances={balances} accounts={accounts} />
            <StepNav onNext={() => go('list')} nextLabel={t('newjob.step.next')} nextDisabled={sender === null} nextHint={t('newjob.sender.needChoice')} />
          </>,
        )}

      {step === 'list' &&
        frame(
          <>
            <ListStep
              kind={listKind}
              onKind={setListKind}
              csv={csv}
              verification={verification}
              onVerification={setVerification}
              unknownChoice={unknownChoice}
              onUnknownChoice={setUnknownChoice}
            />
            <StepNav onBack={() => go('sender')} onNext={() => go('message')} nextLabel={t('newjob.step.next')} nextDisabled={!listReady} nextHint={listHint} />
          </>,
        )}

      {step === 'message' &&
        frame(
          <MessageStep
            message={message}
            onChange={edit}
            columns={columns}
            hasColumns={listKind === 'csv' || columns.length > 0}
            paletteNote={listKind === 'verification' && columns.length > 0 ? t('newjob.palette.verifiedNote') : undefined}
            serverProblem={serverProblem}
            tooLong={tooLong}
            onBack={() => go('list')}
            onNext={toPreview}
          />,
        )}

      {step === 'preview' &&
        frame(
          <ReviewSection
            draft={draft}
            expect="send"
            balances={balances}
            settingsOptedOut={null}
            onRestart={editMessage}
            restartLabel={t('newjob.send.editMessage')}
            fileHint={t('newjob.upload.checkFile')}
          />,
        )}
    </section>
  )
}

// New verification job (/new/verify): choose a CSV file, say which column is the address, look at the preview (rows, what is left out and why, exact credits, the
// line about the outside service), then confirm now or for later. A draft the user left can be continued with `?job=<id>`.
//
// The file goes to the platform as the File itself (streamed from disk), with `email_column` naming the chosen column. Nothing else about the file is sent.
import { useEffect, useState } from 'react'
import { jobsApi, settingsApi } from '../../api/endpoints.ts'
import { t } from '../../copy/index.ts'
import { navigate, useRoute } from '../../router.tsx'
import { Button, Card, PageHeader } from '../../ui/index.ts'
import { CsvStep } from './CsvStep.tsx'
import { ReviewSection } from './ReviewSection.tsx'
import { useCsvSource, type CsvSource } from './useCsvSource.ts'
import { useDraft } from './useDraft.ts'

/** The user's opt-out as it is now (GET /v1/settings), or null when it could not be read. */
function useSettingsOptedOut(): boolean | null {
  const [optedOut, setOptedOut] = useState<boolean | null>(null)
  useEffect(() => {
    const c = new AbortController()
    settingsApi.get({ signal: c.signal }).then(
      (s) => {
        if (!c.signal.aborted) setOptedOut(s.vendor_fallback_opt_out)
      },
      () => undefined,
    )
    return () => c.abort()
  }, [])
  return optedOut
}

function whyNotReady(source: CsvSource): string {
  if (source.file === null) return t('newjob.verify.needFile')
  if (source.reading || source.head === null) return t('newjob.verify.waitRead')
  if (source.chosen === null) return t('newjob.verify.needColumn')
  return t('newjob.verify.fixProblems')
}

export function NewVerifyPage() {
  const { query } = useRoute()
  const jobParam = query.get('job')
  const draft = useDraft()
  const source = useCsvSource()
  const settingsOptedOut = useSettingsOptedOut()
  const { load } = draft

  // A draft continued from the jobs page: load it and look at it again.
  useEffect(() => {
    if (jobParam !== null && jobParam !== '') void load(jobParam, (j) => j.module === 'm1')
  }, [jobParam, load])

  const continuing = jobParam !== null && jobParam !== ''
  const untouched = draft.job === null && draft.busy === null && draft.error === null

  const check = () => {
    const { file, head, chosen } = source
    if (!source.ready || file === null || head === null || chosen === null) return
    const column = head.header[chosen] ?? ''
    void draft.start((signal) => jobsApi.createCsv(file, { module: 'm1', email_column: column }, { signal }))
  }

  const restart = () => {
    void draft.discard()
    source.clear()
    if (continuing) navigate('/new/verify', { replace: true })
  }

  return (
    <section className="flex flex-col gap-4">
      <PageHeader title={t('newjob.verify.title')} description={continuing ? t('newjob.verify.introContinue') : t('newjob.verify.intro')} />

      {!continuing && draft.result === null && (
        <Card title={t('newjob.verify.listTitle')}>
          <div className="flex flex-col gap-4">
            <CsvStep source={source} purpose="verify" locked={!untouched} />
            {untouched && (
              <div className="flex flex-wrap items-center gap-3">
                <Button variant="primary" disabled={!source.ready} onClick={check}>
                  {t('newjob.verify.check')}
                </Button>
                {!source.ready && <span className="text-sm text-fg-muted">{whyNotReady(source)}</span>}
              </div>
            )}
          </div>
        </Card>
      )}

      <ReviewSection
        draft={draft}
        expect="verify"
        balances={null}
        settingsOptedOut={settingsOptedOut}
        onRestart={restart}
        restartLabel={continuing ? t('newjob.verify.startOver') : t('newjob.csv.change')}
        fileHint={t('newjob.upload.checkFile')}
      />
    </section>
  )
}

// The preview of a job in words and figures: what is in the list, what will be done with it, what is left out and why, and what it costs. The platform computes
// every number (Docs/jobs-api.md, "The job"); this only says them. Nothing the platform says in its own words is shown: a malformed row's reason, a verdict, a
// missing field are each turned into a sentence of the catalogue (copy/words.ts), and an unknown word into a generic one.
import type { ReactNode } from 'react'
import type { Balances, Job, Preview } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { malformedReasonText, resultStateText } from '../../copy/words.ts'
import { Link } from '../../router.tsx'
import { Card } from '../../ui/index.ts'
import { fmt, formatWhen } from './schedule.ts'

/** One figure: its label, its value, and (a second description) what to know about it. A definition list, so a screen reader reads each label with its figure. */
function Stat({ label, value, strong = false, children }: { label: string; value: ReactNode; strong?: boolean; children?: ReactNode }) {
  return (
    <div className="grid grid-cols-[1fr_auto] items-baseline gap-x-4 border-b border-border py-1.5">
      <dt className="text-base text-fg">{label}</dt>
      <dd className={strong ? 'm-0 text-base font-semibold text-fg tabular-nums' : 'm-0 text-base text-fg tabular-nums'}>{value}</dd>
      {children !== undefined && children !== null && children !== false && <dd className="m-0 col-span-2 mt-0.5 text-sm text-fg-muted">{children}</dd>}
    </div>
  )
}

function Group({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="flex flex-col">
      <h3 className="mb-1 text-base font-semibold text-fg">{title}</h3>
      <dl className="m-0 flex flex-col">{children}</dl>
    </section>
  )
}

/** The reasons rows were found malformed, most common first, each as the sentence of the catalogue. */
export function malformedLines(reasons: Record<string, number>): Array<{ reason: string; count: number; text: string }> {
  return Object.entries(reasons)
    .filter(([, count]) => count > 0)
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([reason, count]) => ({ reason, count, text: malformedReasonText(reason) }))
}

const VERDICTS = ['valid', 'risky', 'invalid', 'unknown'] as const

function Verdicts({ verdicts }: { verdicts: Record<string, number> }) {
  return (
    <Stat label={t('newjob.preview.fromVerification')} value={fmt(VERDICTS.reduce((n, v) => n + (verdicts[v] ?? 0), 0))}>
      <ul className="m-0 flex list-none flex-wrap gap-x-4 p-0">
        {VERDICTS.map((v) => (
          <li key={v}>
            {resultStateText(v)}: <span className="tabular-nums">{fmt(verdicts[v] ?? 0)}</span>
          </li>
        ))}
      </ul>
    </Stat>
  )
}

/** What the money or the entitlement is, for the kind of job. */
function Cost({ job, preview, balances }: { job: Job; preview: Preview; balances: Balances | null | undefined }) {
  if (!preview.metered) {
    const m3 = balances?.m3
    return (
      <Group title={t('newjob.preview.costTitle')}>
        <Stat label={t('newjob.preview.noCredits')} value={t('common.none')}>
          {m3 === undefined ? null : m3.state === 'suspended' ? (
            <>
              {t('newjob.entitlement.suspended')} <Link to="/accounts">{t('newjob.entitlement.link')}</Link>
            </>
          ) : !m3.enabled ? (
            <>
              {t('newjob.entitlement.off')} <Link to="/accounts">{t('newjob.entitlement.link')}</Link>
            </>
          ) : m3.until !== null ? (
            t('newjob.entitlement.until', { date: formatWhen(m3.until) })
          ) : (
            t('newjob.entitlement.on')
          )}
        </Stat>
      </Group>
    )
  }
  return (
    <Group title={t('newjob.preview.costTitle')}>
      <Stat label={t('newjob.preview.credits')} value={fmt(preview.credits)} strong>
        {t(job.module === 'm1' ? 'newjob.preview.unitCostCheck' : 'newjob.preview.unitCostSend', { cost: fmt(preview.unit_cost) })}
      </Stat>
      <Stat label={t('newjob.preview.available')} value={fmt(preview.available)} />
      {preview.shortfall > 0 && (
        <Stat label={t('newjob.preview.shortfall')} value={fmt(preview.shortfall)} strong>
          <span className="font-medium text-danger">{t('newjob.preview.shortfallNext')}</span>
        </Stat>
      )}
    </Group>
  )
}

export interface PreviewPanelProps {
  job: Job
  preview: Preview
  /** the credits and the entitlement, for the cost of a send (null when they could not be read) */
  balances?: Balances | null
  /** shown at the end of a verification's figures: the sentence about the outside service */
  disclosure?: ReactNode
}

export function PreviewPanel({ job, preview, balances, disclosure }: PreviewPanelProps) {
  const isVerify = job.module === 'm1'
  const malformed = malformedLines(preview.malformed_reasons)
  const chained = job.source_job_id !== null

  return (
    <Card title={t('newjob.preview.title')}>
      <div className="flex flex-col gap-5">
        <Group title={t('newjob.preview.listTitle')}>
          <Stat label={t('newjob.preview.rows')} value={fmt(preview.rows)} />
          {preview.verdicts !== undefined && <Verdicts verdicts={preview.verdicts} />}
          <Stat label={t('newjob.preview.malformed')} value={fmt(preview.malformed)}>
            {malformed.length > 0 && (
              <ul className="m-0 flex list-none flex-col gap-0.5 p-0">
                {malformed.map((m) => (
                  <li key={m.reason}>
                    <span className="tabular-nums">{fmt(m.count)}</span> {m.text}
                  </li>
                ))}
              </ul>
            )}
          </Stat>
          <Stat label={t('newjob.preview.duplicates')} value={fmt(preview.duplicates)} />
          {(chained || preview.excluded > 0) && <Stat label={t('newjob.preview.excluded')} value={fmt(preview.excluded)} />}
          <Stat label={isVerify ? t('newjob.preview.suppressedVerify') : t('newjob.preview.suppressedSend')} value={fmt(preview.suppressed)}>
            {isVerify && preview.suppressed > 0 && preview.suppressed_still_verified ? t('newjob.preview.suppressedVerifyNote') : null}
          </Stat>
          {!isVerify && (
            <>
              <Stat label={t('newjob.preview.missing')} value={fmt(preview.missing_fields)}>
                {preview.missing_fields > 0 ? (
                  <>
                    {t('newjob.preview.missingNames')} <span className="font-mono text-fg">{preview.missing_field_names.join(', ')}</span>
                    {preview.missing_field_names_truncated && <> {t('newjob.preview.missingMore')}</>}
                  </>
                ) : null}
              </Stat>
              <Stat label={t('newjob.preview.unrenderable')} value={fmt(preview.unrenderable)}>
                {preview.unrenderable > 0 ? t('newjob.preview.unrenderableNote') : null}
              </Stat>
            </>
          )}
        </Group>

        <Group title={t('newjob.preview.willTitle')}>
          <Stat label={isVerify ? t('newjob.preview.unitsVerify') : t('newjob.preview.unitsSend')} value={fmt(preview.units)} strong />
          {chained && <Stat label={t('newjob.preview.selected')} value={fmt(preview.selected)} />}
        </Group>
        {preview.units === 0 && <p className="text-sm font-medium text-danger">{t('newjob.preview.nothing')}</p>}

        <Cost job={job} preview={preview} balances={balances} />

        {!isVerify && <p className="text-sm text-fg-muted">{t('newjob.preview.unsubscribe')}</p>}
        {disclosure}
      </div>
    </Card>
  )
}

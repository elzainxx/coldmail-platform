// The sample messages of a send's preview: real rows rendered exactly as they will be sent (the first, the middle and the last of the rows that would be sent).
// Each is a card with its subject, its plain text and its HTML in the sandboxed frame (SandboxedFrame.tsx: the only way this app shows a user's HTML).
// They hold the recipients' data and exist only in the answer of the preview request: they are shown here and kept nowhere.
import type { Sample } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { Card, CodeBlock } from '../../ui/index.ts'
import { fmt } from './schedule.ts'
import { SandboxedFrame } from './SandboxedFrame.tsx'

export function Samples({ samples }: { samples: readonly Sample[] }) {
  if (samples.length === 0) return null
  return (
    <Card title={t('newjob.samples.title')}>
      <p className="mb-3 text-sm text-fg-muted">{t('newjob.samples.intro')}</p>
      <div className="flex flex-col gap-4">
        {samples.map((sample, i) => {
          const n = i + 1
          return (
            <section key={sample.ordinal} data-testid={`sample-${n}`} aria-label={t('newjob.samples.sample', { n, row: fmt(sample.ordinal) })} className="rounded-lg border border-border p-3">
              <h3 className="text-base font-semibold text-fg">{t('newjob.samples.sample', { n, row: fmt(sample.ordinal) })}</h3>
              <dl className="m-0 mt-2 flex flex-col gap-3">
                <div>
                  <dt className="text-sm font-medium text-fg-muted">{t('newjob.samples.subject')}</dt>
                  <dd className="m-0 text-base text-fg">{sample.subject}</dd>
                </div>
                <div>
                  <dt className="mb-1 text-sm font-medium text-fg-muted">{t('newjob.samples.html')}</dt>
                  <dd className="m-0">
                    {sample.html === '' ? (
                      <p className="text-sm text-fg-muted">{t('newjob.samples.noHtml')}</p>
                    ) : (
                      <SandboxedFrame html={sample.html} title={t('newjob.samples.htmlOf', { n })} />
                    )}
                  </dd>
                </div>
                <div>
                  <dt className="mb-1 text-sm font-medium text-fg-muted">{t('newjob.samples.text')}</dt>
                  <dd className="m-0">
                    {sample.text === '' ? (
                      <p className="text-sm text-fg-muted">{t('newjob.samples.noText')}</p>
                    ) : (
                      <CodeBlock label={t('newjob.samples.textOf', { n })}>{sample.text}</CodeBlock>
                    )}
                  </dd>
                </div>
              </dl>
            </section>
          )
        })}
      </div>
    </Card>
  )
}

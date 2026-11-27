// The help for the grammar, in plain words: merge fields and their fallbacks, choices of words, escapes, and why a brace in a style block must be escaped.
// A native disclosure (<details>), so it opens and closes with the keyboard and a screen reader says whether it is open. The sentences are the catalogue's;
// the examples are syntax (grammar.ts).
import { t } from '../../../copy/index.ts'
import { Card } from '../../../ui/index.ts'
import { GRAMMAR_EXAMPLES } from './grammar.ts'

function Topic({ title, children }: { title: string; children: string }) {
  return (
    <div>
      <h4 className="text-base font-semibold text-fg">{title}</h4>
      <p className="text-sm text-fg-muted">{children}</p>
    </div>
  )
}

export function GrammarHelp() {
  return (
    <Card>
      <details>
        <summary className="cursor-pointer text-base font-semibold text-fg">{t('newjob.help.summary')}</summary>
        <div className="mt-3 flex flex-col gap-4">
          <Topic title={t('newjob.help.fieldsTitle')}>{t('newjob.help.fields')}</Topic>
          <Topic title={t('newjob.help.fallbackTitle')}>{t('newjob.help.fallback')}</Topic>
          <Topic title={t('newjob.help.reservedTitle')}>{t('newjob.help.reserved')}</Topic>
          <Topic title={t('newjob.help.choicesTitle')}>{t('newjob.help.choices')}</Topic>
          <Topic title={t('newjob.help.escapesTitle')}>{t('newjob.help.escapes')}</Topic>
          <Topic title={t('newjob.help.bracesTitle')}>{t('newjob.help.braces')}</Topic>
          <Topic title={t('newjob.help.textTitle')}>{t('newjob.help.text')}</Topic>
          <Topic title={t('newjob.help.unsubscribeTitle')}>{t('newjob.help.unsubscribe')}</Topic>
          <div className="overflow-x-auto">
            <table className="w-full border-separate border-spacing-0 text-left text-sm">
              <caption className="pb-1 text-left text-sm font-medium text-fg">{t('newjob.help.examples')}</caption>
              <thead>
                <tr>
                  <th scope="col" className="border-b border-border bg-sunken px-3 py-1.5 text-xs font-medium text-fg-muted">
                    {t('newjob.help.youWrite')}
                  </th>
                  <th scope="col" className="border-b border-border bg-sunken px-3 py-1.5 text-xs font-medium text-fg-muted">
                    {t('newjob.help.youGet')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {GRAMMAR_EXAMPLES.map((ex) => (
                  <tr key={ex.write}>
                    <td className="border-b border-border px-3 py-1.5 align-top">
                      <code className="font-mono text-fg">{ex.write}</code>
                    </td>
                    <td className="border-b border-border px-3 py-1.5 align-top text-fg">{t(ex.gives)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </details>
    </Card>
  )
}

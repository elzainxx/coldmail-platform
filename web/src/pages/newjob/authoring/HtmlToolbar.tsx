// The tags a person writing an email in HTML reaches for, as buttons above the HTML box: each wraps the selected text (or leaves the caret inside the new tag).
// No third-party editor (decision 3 of the spec): this is a text box and some buttons. The Unsubscribe link inserts the link every message carries.
import { t } from '../../../copy/index.ts'
import { Button } from '../../../ui/index.ts'
import type { Snippet } from './text.ts'

interface Tool {
  id: string
  label: string
  snippet: Snippet
}

function tools(): Tool[] {
  return [
    { id: 'paragraph', label: t('newjob.authoring.tag.paragraph'), snippet: { before: '<p>', after: '</p>' } },
    { id: 'heading', label: t('newjob.authoring.tag.heading'), snippet: { before: '<h2>', after: '</h2>' } },
    { id: 'bold', label: t('newjob.authoring.tag.bold'), snippet: { before: '<strong>', after: '</strong>' } },
    { id: 'italic', label: t('newjob.authoring.tag.italic'), snippet: { before: '<em>', after: '</em>' } },
    { id: 'link', label: t('newjob.authoring.tag.link'), snippet: { before: '<a href="https://">', after: '</a>', placeholder: t('newjob.authoring.snippet.linkText') } },
    { id: 'break', label: t('newjob.authoring.tag.lineBreak'), snippet: { before: '<br>', replace: true } },
    { id: 'list', label: t('newjob.authoring.tag.list'), snippet: { before: '<ul>\n  <li>', after: '</li>\n</ul>' } },
    {
      id: 'unsubscribe',
      label: t('newjob.authoring.tag.unsubscribe'),
      snippet: { before: '<a href="{{unsubscribe_url}}">', after: '</a>', placeholder: t('newjob.authoring.snippet.unsubscribeText') },
    },
  ]
}

export function HtmlToolbar({ onInsert }: { onInsert: (snippet: Snippet) => void }) {
  return (
    <div role="group" aria-label={t('newjob.authoring.toolbar')} className="flex flex-wrap gap-1.5">
      {tools().map((tool) => (
        <Button key={tool.id} size="sm" onClick={() => onInsert(tool.snippet)}>
          {tool.label}
        </Button>
      ))}
    </div>
  )
}

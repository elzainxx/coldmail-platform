import { useState } from 'react'
import { t } from '../copy/index.ts'
import { Button } from './Button.tsx'
import { cx } from './cx.ts'

export interface CodeBlockProps {
  /** The text. Shown as it is (never as HTML), in a monospace block that scrolls. */
  children: string
  /** What the text is ("The message as it will be sent"): the accessible name of the block. */
  label?: string
  /** a Copy button */
  copyable?: boolean
  className?: string
}

/** Read-only text in a block: focusable so the keyboard can scroll it, and copyable. */
export function CodeBlock({ children, label, copyable = false, className }: CodeBlockProps) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(children)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      // the browser refused (no permission, an insecure page): the text can still be selected by hand
    }
  }
  return (
    <div className={cx('flex flex-col gap-2', className)}>
      <pre
        tabIndex={0}
        role="region"
        aria-label={label ?? t('common.codeBlock.label')}
        className="max-h-96 overflow-auto rounded-md border border-border bg-sunken p-3 font-mono text-sm whitespace-pre-wrap text-fg"
      >
        <code>{children}</code>
      </pre>
      {copyable && (
        <div className="flex items-center gap-2">
          <Button size="sm" onClick={copy}>
            {t('common.copy')}
          </Button>
          <span role="status" className="text-xs text-fg-muted">
            {copied ? t('common.copied') : ''}
          </span>
        </div>
      )}
    </div>
  )
}

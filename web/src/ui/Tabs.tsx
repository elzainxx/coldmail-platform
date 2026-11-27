import { useId, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import { cx } from './cx.ts'

export interface TabDef {
  /** unique within the tabs */
  id: string
  label: string
  panel: ReactNode
}

export interface TabsProps {
  /** What the tabs choose between ("Job details"): the accessible name of the tab list. */
  label: string
  tabs: readonly TabDef[]
  /** controlled: the id of the selected tab */
  value?: string
  defaultValue?: string
  onChange?: (id: string) => void
  className?: string
}

/** Tabs by the WAI-ARIA pattern: one tab stop for the list, the arrow keys, Home and End move between tabs (and select them), the panel is focusable. */
export function Tabs({ label, tabs, value, defaultValue, onChange, className }: TabsProps) {
  const base = useId()
  const [inner, setInner] = useState(defaultValue ?? tabs[0]?.id ?? '')
  const selected = value ?? inner
  const refs = useRef<Map<string, HTMLButtonElement>>(new Map())

  const select = (id: string, focus: boolean) => {
    if (value === undefined) setInner(id)
    onChange?.(id)
    if (focus) refs.current.get(id)?.focus()
  }

  const onKeyDown = (e: KeyboardEvent<HTMLButtonElement>, index: number) => {
    let next: number | null = null
    if (e.key === 'ArrowRight') next = (index + 1) % tabs.length
    else if (e.key === 'ArrowLeft') next = (index - 1 + tabs.length) % tabs.length
    else if (e.key === 'Home') next = 0
    else if (e.key === 'End') next = tabs.length - 1
    if (next === null) return
    e.preventDefault()
    const target = tabs[next]
    if (target) select(target.id, true)
  }

  const current = tabs.find((t) => t.id === selected) ?? tabs[0]
  return (
    <div className={className}>
      <div role="tablist" aria-label={label} className="flex gap-1 border-b border-border">
        {tabs.map((tab, i) => {
          const isSelected = tab.id === current?.id
          return (
            <button
              key={tab.id}
              ref={(el) => {
                if (el) refs.current.set(tab.id, el)
                else refs.current.delete(tab.id)
              }}
              type="button"
              role="tab"
              id={`${base}-tab-${tab.id}`}
              aria-selected={isSelected}
              aria-controls={`${base}-panel-${tab.id}`}
              tabIndex={isSelected ? 0 : -1}
              onClick={() => select(tab.id, false)}
              onKeyDown={(e) => onKeyDown(e, i)}
              className={cx(
                '-mb-px cursor-pointer border-b-2 px-3 py-1.5 text-sm font-medium transition-colors',
                isSelected ? 'border-accent text-fg' : 'border-transparent text-fg-muted hover:text-fg',
              )}
            >
              {tab.label}
            </button>
          )
        })}
      </div>
      {current && (
        <div role="tabpanel" id={`${base}-panel-${current.id}`} aria-labelledby={`${base}-tab-${current.id}`} tabIndex={0} className="pt-3">
          {current.panel}
        </div>
      )}
    </div>
  )
}

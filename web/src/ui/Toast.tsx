// Short messages that appear without taking focus ("Job paused", "The address was added"). The region is in the page from the start and is a polite live
// region, so a screen reader announces each toast as it arrives; an error toast has role="alert" and is announced at once.
//
//   const toast = useToast()
//   toast.notify(t('suppressions.added'))                       // 5 seconds
//   toast.notify(errorText(err), { tone: 'error' })             // stays 10 seconds
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { t } from '../copy/index.ts'
import { cx } from './cx.ts'
import { CloseIcon } from './icons.tsx'
import { IconButton } from './IconButton.tsx'

export type ToastTone = 'info' | 'success' | 'error'

export interface ToastOptions {
  tone?: ToastTone
  /** milliseconds on screen; 0 keeps it until dismissed */
  timeoutMs?: number
}

interface ToastItem {
  id: number
  message: string
  tone: ToastTone
}

export interface ToastApi {
  notify: (message: string, options?: ToastOptions) => number
  dismiss: (id: number) => void
}

const ToastContext = createContext<ToastApi | null>(null)

const MAX_VISIBLE = 4

const TONE_CLASS: Record<ToastTone, string> = {
  info: 'border-border bg-raised text-fg',
  success: 'border-transparent bg-status-valid-bg text-status-valid',
  error: 'border-transparent bg-status-invalid-bg text-status-invalid',
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([])
  const timers = useRef(new Map<number, ReturnType<typeof setTimeout>>())
  const nextId = useRef(1)

  const dismiss = useCallback((id: number) => {
    const timer = timers.current.get(id)
    if (timer !== undefined) clearTimeout(timer)
    timers.current.delete(id)
    setItems((list) => list.filter((i) => i.id !== id))
  }, [])

  const notify = useCallback(
    (message: string, options: ToastOptions = {}) => {
      const id = nextId.current++
      const tone = options.tone ?? 'info'
      const timeoutMs = options.timeoutMs ?? (tone === 'error' ? 10_000 : 5_000)
      setItems((list) => [...list, { id, message, tone }].slice(-MAX_VISIBLE))
      if (timeoutMs > 0) timers.current.set(id, setTimeout(() => dismiss(id), timeoutMs))
      return id
    },
    [dismiss],
  )

  useEffect(() => {
    const live = timers.current
    return () => {
      for (const timer of live.values()) clearTimeout(timer)
      live.clear()
    }
  }, [])

  const api = useMemo<ToastApi>(() => ({ notify, dismiss }), [notify, dismiss])

  return (
    <ToastContext value={api}>
      {children}
      <div role="region" aria-label={t('common.toast.region')} aria-live="polite" className="pointer-events-none fixed right-4 bottom-4 z-[60] flex w-80 max-w-[calc(100vw-2rem)] flex-col gap-2">
        {items.map((item) => (
          <div
            key={item.id}
            role={item.tone === 'error' ? 'alert' : undefined}
            className={cx('pointer-events-auto flex items-start justify-between gap-2 rounded-md border px-3 py-2 text-base shadow-md', TONE_CLASS[item.tone])}
          >
            <p className="min-w-0 py-1">{item.message}</p>
            <IconButton label={t('common.toast.dismiss')} onClick={() => dismiss(item.id)} className="size-7">
              <CloseIcon />
            </IconButton>
          </div>
        ))}
      </div>
    </ToastContext>
  )
}

export function useToast(): ToastApi {
  const ctx = useContext(ToastContext)
  if (ctx === null) throw new Error('useToast must be used inside <ToastProvider>')
  return ctx
}

import { t } from '../copy/index.ts'

/** The first thing a keyboard user can reach: it jumps over the navigation to the page. Hidden until it has focus. */
export function SkipLink() {
  return (
    <a
      href="#main"
      onClick={(e) => {
        e.preventDefault() // no #main left in the address bar for the router to trip over
        document.getElementById('main')?.focus()
      }}
      className="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-[70] focus:rounded-md focus:bg-accent focus:px-3 focus:py-2 focus:text-on-accent focus:no-underline"
    >
      {t('common.skipToContent')}
    </a>
  )
}

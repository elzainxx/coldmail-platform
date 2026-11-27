// The theme: light, dark, or (nothing stored) whatever the system prefers. The only thing this app stores in the browser, and the only file allowed to touch
// localStorage (auth.test.ts scans every other source file for a storage call). It stores one word, "light" or "dark", never anything about the user.
//
// The system preference needs no code: tokens.css follows prefers-color-scheme. A stored choice sets data-theme on <html>, which tokens.css puts first.
export type Theme = 'light' | 'dark'

const KEY = 'coldmail-theme'

export function storedTheme(): Theme | null {
  try {
    const v = window.localStorage.getItem(KEY)
    return v === 'light' || v === 'dark' ? v : null
  } catch {
    return null // storage blocked (a private window): the system preference stands
  }
}

/** The theme in force now: the one chosen, else the system's. */
export function currentTheme(): Theme {
  const set = document.documentElement.getAttribute('data-theme')
  if (set === 'light' || set === 'dark') return set
  return window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
}

/** Put a theme on the page (null: back to the system's). Does not store it. */
export function applyTheme(theme: Theme | null): void {
  if (theme === null) document.documentElement.removeAttribute('data-theme')
  else document.documentElement.setAttribute('data-theme', theme)
}

/** The user's choice: applied now and kept for the next visit. */
export function chooseTheme(theme: Theme): void {
  applyTheme(theme)
  try {
    window.localStorage.setItem(KEY, theme)
  } catch {
    // the choice holds for this visit only
  }
}

/** At start, before the first render. */
export function initTheme(): void {
  applyTheme(storedTheme())
}

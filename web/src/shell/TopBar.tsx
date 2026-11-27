import { useEffect, useState } from 'react'
import { meApi } from '../api/endpoints.ts'
import { SIGN_IN_PATH } from '../auth/paths.ts'
import { session } from '../auth/session.ts'
import { t } from '../copy/index.ts'
import { Link, navigate } from '../router.tsx'
import { Button, IconButton, MenuIcon, CloseIcon } from '../ui/index.ts'
import { ThemeToggle } from './ThemeToggle.tsx'

export interface TopBarProps {
  menuOpen: boolean
  onToggleMenu: () => void
  /** the id of the navigation the menu button opens */
  navId: string
}

/** The bar at the top of every signed-in page: the name, the user, the theme, sign out. */
export function TopBar({ menuOpen, onToggleMenu, navId }: TopBarProps) {
  const [email, setEmail] = useState<string | null>(null)

  useEffect(() => {
    // GET /v1/me names the user. A service without the route (or a failure) just leaves the name out: the bar works without it.
    const controller = new AbortController()
    meApi
      .get({ signal: controller.signal })
      .then((me) => {
        if (typeof me?.email === 'string' && me.email !== '') setEmail(me.email)
      })
      .catch(() => {})
    return () => controller.abort()
  }, [])

  const signOut = () => {
    void session.signOut() // wipes the tokens at once (GoTrue is told afterwards) ...
    navigate(SIGN_IN_PATH, { replace: true }) // ... so the sign-in page is shown with nothing of the user left
  }

  return (
    <header className="flex h-12 shrink-0 items-center gap-3 border-b border-border bg-raised px-4">
      <IconButton
        label={menuOpen ? t('common.menu.close') : t('common.menu.open')}
        aria-expanded={menuOpen}
        aria-controls={navId}
        onClick={onToggleMenu}
        className="md:hidden"
      >
        {menuOpen ? <CloseIcon /> : <MenuIcon />}
      </IconButton>
      <Link to="/" className="text-lg font-semibold text-fg no-underline hover:text-fg">
        {t('common.brand')}
      </Link>
      <div className="ml-auto flex items-center gap-2">
        {email !== null && (
          <span className="hidden max-w-64 truncate text-sm text-fg-muted sm:inline" title={email}>
            {t('common.signedInAs', { email })}
          </span>
        )}
        <ThemeToggle />
        <Button variant="secondary" size="sm" onClick={signOut}>
          {t('common.signOut')}
        </Button>
      </div>
    </header>
  )
}

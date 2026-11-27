// The two frames a page sits in. Both give the page one <main id="main"> (the target of the skip link and the fallback of the router's focus), the
// offline banner and an error boundary that lets the next route try again.
//   AppFrame   a signed-in user's pages: the top bar, the navigation, the page
//   AuthFrame  everything else (sign in, forgot password, set a password, not found while signed out): a centred column and nothing about the user
import { useState } from 'react'
import { useSession } from '../auth/useSession.ts'
import { t } from '../copy/index.ts'
import type { LayoutProps } from '../router.tsx'
import { ErrorBoundary } from './ErrorBoundary.tsx'
import { Nav } from './Nav.tsx'
import { OfflineBanner } from './OfflineBanner.tsx'
import { SkipLink } from './SkipLink.tsx'
import { ThemeToggle } from './ThemeToggle.tsx'
import { TopBar } from './TopBar.tsx'

const NAV_ID = 'site-nav'

function AppFrame({ pathname, children }: Pick<LayoutProps, 'pathname' | 'children'>) {
  const [menuOpen, setMenuOpen] = useState(false)
  return (
    <>
      <SkipLink />
      <TopBar menuOpen={menuOpen} onToggleMenu={() => setMenuOpen((o) => !o)} navId={NAV_ID} />
      <OfflineBanner />
      <div className="mx-auto flex w-full max-w-7xl flex-1 flex-col gap-4 px-4 py-4 md:flex-row md:gap-6">
        <Nav id={NAV_ID} className={`${menuOpen ? 'block' : 'hidden'} md:block md:w-48 md:shrink-0`} onNavigate={() => setMenuOpen(false)} />
        <main id="main" tabIndex={-1} className="min-w-0 flex-1">
          <ErrorBoundary resetKey={pathname}>{children}</ErrorBoundary>
        </main>
      </div>
    </>
  )
}

function AuthFrame({ pathname, children }: Pick<LayoutProps, 'pathname' | 'children'>) {
  return (
    <>
      <SkipLink />
      <OfflineBanner />
      <div className="flex items-center justify-between px-4 py-3">
        <span className="text-lg font-semibold text-fg">{t('common.brand')}</span>
        <ThemeToggle />
      </div>
      <main id="main" tabIndex={-1} className="mx-auto flex w-full max-w-md flex-1 flex-col justify-center px-4 pb-16">
        <ErrorBoundary resetKey={pathname}>{children}</ErrorBoundary>
      </main>
    </>
  )
}

/** The layout the Router uses: the app's frame for a signed-in user on a page of the app, the plain column for everything else. */
export function ShellLayout({ route, pathname, children }: LayoutProps) {
  const snapshot = useSession()
  const inApp = snapshot.status === 'signed-in' && (route === null || route.protected)
  return inApp ? <AppFrame pathname={pathname}>{children}</AppFrame> : <AuthFrame pathname={pathname}>{children}</AuthFrame>
}

// The app: what is set up once (the settings from /config.json, the sign-in client, the browser listeners), then the router.
//
//   booting   a spinner while /config.json loads (nothing of the app is rendered, so nothing can ask for data before the session is ready)
//   failed    a calm message and a Try again button (the app cannot sign anyone in without the config)
//   ready     <Router>: the guard decides, per route, whether the page is rendered at all
import { useCallback, useEffect, useRef, useState } from 'react'
import { watchBrowserConnectivity } from './api/connectivity.ts'
import type { AppConfig } from './api/types.ts'
import { loadConfig } from './auth/config.ts'
import { createGoTrue } from './auth/gotrue.ts'
import { guardRoute } from './auth/guard.ts'
import type { AuthLanding } from './auth/hash.ts'
import { installPageshowGuard } from './auth/lifecycle.ts'
import { ACCEPT_INVITE_PATH, RESET_PASSWORD_PATH, SIGN_IN_PATH } from './auth/paths.ts'
import { session } from './auth/session.ts'
import { useSession } from './auth/useSession.ts'
import { t } from './copy/index.ts'
import { NotFoundPage } from './pages/NotFound.tsx'
import { navigate, Router, type LocationInfo, type RouteDef } from './router.tsx'
import { routes } from './routes.ts'
import { ErrorBoundary } from './shell/ErrorBoundary.tsx'
import { ShellLayout } from './shell/Layout.tsx'
import { Button, Spinner, ToastProvider } from './ui/index.ts'

export interface AppProps {
  /** what the address bar held at start when it was a sign-in link (auth/hash.ts consumeAuthHash); main.tsx passes it */
  landing?: AuthLanding | null
  /** how the config is fetched (tests pass a fake) */
  load?: () => Promise<AppConfig>
}

type Boot = 'booting' | 'ready' | 'failed'

function Routed() {
  const snapshot = useSession()
  const guard = useCallback((route: RouteDef | null, location: LocationInfo) => guardRoute(snapshot, route, location), [snapshot])
  return <Router routes={routes} guard={guard} notFound={NotFoundPage} layout={ShellLayout} fallback={<Spinner />} />
}

export function App({ landing = null, load = loadConfig }: AppProps) {
  const [boot, setBoot] = useState<Boot>(session.isConfigured() ? 'ready' : 'booting')
  const landed = useRef(false)

  // An invitation or reset link: keep its session in memory and go and choose the password. A dead link: say so on the sign-in page. Once only.
  const applyLanding = useCallback(() => {
    if (landed.current) return
    landed.current = true
    if (landing?.kind === 'session') {
      session.adopt(landing.tokens, landing.setup)
      navigate(landing.setup === 'invite' ? ACCEPT_INVITE_PATH : RESET_PASSWORD_PATH, { replace: true })
    } else if (landing?.kind === 'error') {
      navigate(`${SIGN_IN_PATH}?notice=link_expired`, { replace: true })
    }
  }, [landing])

  const start = useCallback(() => {
    setBoot('booting')
    load().then(
      (config) => {
        session.configure(createGoTrue(config))
        applyLanding()
        setBoot('ready')
      },
      () => setBoot('failed'),
    )
  }, [load, applyLanding])

  useEffect(() => {
    const stopConnectivity = watchBrowserConnectivity()
    const stopPageshow = installPageshowGuard(session)
    return () => {
      stopConnectivity()
      stopPageshow()
    }
  }, [])

  useEffect(() => {
    if (session.isConfigured()) applyLanding()
    else start()
  }, [start, applyLanding])

  return (
    <ErrorBoundary>
      <ToastProvider>
        <div className="flex min-h-screen flex-col bg-surface text-fg">
          {boot === 'ready' && <Routed />}
          {boot === 'booting' && (
            <div className="flex flex-1 items-center justify-center">
              <Spinner size={28} />
            </div>
          )}
          {boot === 'failed' && (
            <main className="mx-auto flex w-full max-w-md flex-1 flex-col justify-center gap-3 px-4">
              <h1 className="text-2xl font-semibold">{t('common.boot.title')}</h1>
              <p className="text-fg-muted">{t('common.boot.body')}</p>
              <div>
                <Button variant="primary" onClick={start}>
                  {t('common.retry')}
                </Button>
              </div>
            </main>
          )}
        </div>
      </ToastProvider>
    </ErrorBoundary>
  )
}

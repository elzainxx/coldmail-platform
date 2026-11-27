// A page that comes back from the browser's back-forward cache (the back button after leaving the site) is the page EXACTLY as it was, tokens and data
// included. So on a `pageshow` with `persisted` the session is wiped and the user is sent to the sign-in page: the back button after a sign out shows the
// sign-in page and nothing else.
import { navigate } from '../router.tsx'
import { SIGN_IN_PATH } from './paths.ts'
import type { Session } from './session.ts'

export function installPageshowGuard(s: Pick<Session, 'clear'>, win: Pick<Window, 'addEventListener' | 'removeEventListener'> = window): () => void {
  const onPageShow = (e: Event) => {
    if ((e as PageTransitionEvent).persisted !== true) return
    s.clear()
    navigate(SIGN_IN_PATH, { replace: true })
  }
  win.addEventListener('pageshow', onPageShow)
  return () => win.removeEventListener('pageshow', onPageShow)
}

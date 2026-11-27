import { useSyncExternalStore } from 'react'
import { session, type SessionSnapshot } from './session.ts'

/** The state of the session (signed out, password required, signed in). It re-renders when the state changes; the tokens are never exposed. */
export function useSession(): SessionSnapshot {
  return useSyncExternalStore(session.subscribe, session.getSnapshot)
}

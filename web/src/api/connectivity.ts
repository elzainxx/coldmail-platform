// What the offline / 503 banner shows. The API client reports every outcome here; the browser's own online and offline events feed it too.
//   ok           the last request got an answer
//   offline      the browser says it is offline, or a request could not be made at all
//   unavailable  the service answered 503 (busy, a module or the sign-in keys unavailable): it is there but not able
export type Connectivity = 'ok' | 'offline' | 'unavailable'

let state: Connectivity = 'ok'
const listeners = new Set<() => void>()

function set(next: Connectivity): void {
  if (next === state) return
  state = next
  for (const l of [...listeners]) l()
}

export function getConnectivity(): Connectivity {
  return state
}

export function subscribeConnectivity(listener: () => void): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

export function reportApiAnswered(status: number): void {
  set(status === 503 ? 'unavailable' : 'ok')
}

export function reportNetworkFailure(): void {
  set('offline')
}

/** Follow the browser's online and offline events. Returns the function that stops following. */
export function watchBrowserConnectivity(win: Pick<Window, 'addEventListener' | 'removeEventListener' | 'navigator'> = window): () => void {
  const onOffline = () => set('offline')
  const onOnline = () => set('ok')
  win.addEventListener('offline', onOffline)
  win.addEventListener('online', onOnline)
  if (win.navigator.onLine === false) set('offline')
  return () => {
    win.removeEventListener('offline', onOffline)
    win.removeEventListener('online', onOnline)
  }
}

/** For tests. */
export function resetConnectivity(): void {
  state = 'ok'
  listeners.clear()
}

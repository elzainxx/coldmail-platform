import { useSyncExternalStore } from 'react'
import { getConnectivity, subscribeConnectivity } from '../api/connectivity.ts'
import { t } from '../copy/index.ts'

/**
 * Says when the app cannot reach the service (offline) or the service says it cannot help right now (503). The live region is always in the page, so
 * a screen reader hears the text when it appears; with nothing to say it takes no room.
 */
export function OfflineBanner() {
  const state = useSyncExternalStore(subscribeConnectivity, getConnectivity)
  const text = state === 'offline' ? t('common.banner.offline') : state === 'unavailable' ? t('common.banner.unavailable') : ''
  return (
    <div role="status" aria-live="polite" className={text === '' ? undefined : 'border-b border-border bg-status-risky-bg px-4 py-2 text-center text-sm text-status-risky'}>
      {text}
    </div>
  )
}

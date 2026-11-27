// The navigation: seven entries, in this order. Each links to a fixed path; the screen agent of the entry's area registers a route at that path
// (src/pages/<area>/routes.ts). A test (shell.test.ts) holds the two together as soon as an area has any route.
import type { CopyKey } from '../copy/index.ts'

export type Area = 'dashboard' | 'jobs' | 'newjob' | 'accounts' | 'suppressions' | 'settings'

export interface NavItem {
  id: string
  to: string
  label: CopyKey
  area: Area
  /** active only on exactly this path (the dashboard), not on the paths below it */
  end?: boolean
}

export const NAV_ITEMS: readonly NavItem[] = [
  { id: 'dashboard', to: '/', label: 'common.nav.dashboard', area: 'dashboard', end: true },
  { id: 'jobs', to: '/jobs', label: 'common.nav.jobs', area: 'jobs' },
  { id: 'new-verify', to: '/new/verify', label: 'common.nav.newVerify', area: 'newjob' },
  { id: 'new-send', to: '/new/send', label: 'common.nav.newSend', area: 'newjob' },
  { id: 'accounts', to: '/accounts', label: 'common.nav.accounts', area: 'accounts' },
  { id: 'suppressions', to: '/suppressions', label: 'common.nav.suppressions', area: 'suppressions' },
  { id: 'settings', to: '/settings', label: 'common.nav.settings', area: 'settings' },
]

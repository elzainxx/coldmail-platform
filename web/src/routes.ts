// THE route registry. The sign-in pages are mine; every other area has its own file, filled by its screen agent:
//
//   src/pages/dashboard/routes.ts      src/pages/newjob/routes.ts        src/pages/suppressions/routes.ts
//   src/pages/jobs/routes.ts           src/pages/accounts/routes.ts      src/pages/settings/routes.ts
//
// This file only joins them. The RouteDef type (path, title as a copy key, element, protected) is in src/router.tsx.
// routes.test.ts holds the registry together: no path twice, every title a key of the catalogue, every protected page marked.
import type { RouteDef } from './router.tsx'
import { routes as accounts } from './pages/accounts/routes.ts'
import { routes as auth } from './pages/auth/routes.ts'
import { routes as dashboard } from './pages/dashboard/routes.ts'
import { routes as jobs } from './pages/jobs/routes.ts'
import { routes as newjob } from './pages/newjob/routes.ts'
import { routes as settings } from './pages/settings/routes.ts'
import { routes as suppressions } from './pages/suppressions/routes.ts'

/** Every area's routes by name (the nav test reads this to check each entry has a page once its area has any). */
export const routesByArea = { auth, dashboard, jobs, newjob, accounts, suppressions, settings } as const

export const routes: RouteDef[] = [...auth, ...dashboard, ...jobs, ...newjob, ...accounts, ...suppressions, ...settings]

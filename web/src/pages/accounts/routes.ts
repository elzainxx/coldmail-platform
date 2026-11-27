// The routes of the 'accounts' area. This file is filled by the screen agent of the area; src/routes.ts only imports it.
//
//   { path: '/jobs/:id',          a path pattern; `:name` is a parameter (useRoute().params.name)
//     title: 'jobs.detail.title', a key of src/copy (document title and screen reader announcement)
//     element: JobDetailPage,     the page component (or a React.lazy one), no props
//     protected: true }           true for every page that shows a user's data: it renders nothing and redirects to sign-in when nobody is signed in
//
// The navigation (src/shell/navItems.ts) links to /accounts: register a route there. Every other path of the area is yours to choose.
import type { RouteDef } from '../../router.tsx'
import { AccountsPage } from './AccountsPage.tsx'

export const routes: RouteDef[] = [{ path: '/accounts', title: 'accounts.title', element: AccountsPage, protected: true }]

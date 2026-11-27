// The routes of the 'dashboard' area. This file is filled by the screen agent of the area; src/routes.ts only imports it.
//
//   { path: '/jobs/:id',          a path pattern; `:name` is a parameter (useRoute().params.name)
//     title: 'jobs.detail.title', a key of src/copy (document title and screen reader announcement)
//     element: JobDetailPage,     the page component (or a React.lazy one), no props
//     protected: true }           true for every page that shows a user's data: it renders nothing and redirects to sign-in when nobody is signed in
//
// The navigation (src/shell/navItems.ts) links to / (the dashboard): register a route there. Every other path of the area is yours to choose.
import type { RouteDef } from '../../router.tsx'
import { DashboardPage } from './DashboardPage.tsx'

export const routes: RouteDef[] = [{ path: '/', title: 'dashboard.title', element: DashboardPage, protected: true }]

// The routes of the 'jobs' area. This file is filled by the screen agent of the area; src/routes.ts only imports it.
//
//   { path: '/jobs/:id',          a path pattern; `:name` is a parameter (useRoute().params.name)
//     title: 'jobs.detail.title', a key of src/copy (document title and screen reader announcement)
//     element: JobDetailPage,     the page component (or a React.lazy one), no props
//     protected: true }           true for every page that shows a user's data: it renders nothing and redirects to sign-in when nobody is signed in
//
// The navigation (src/shell/navItems.ts) links to /jobs (the jobs table). A job's page holds its results too (a page at a time, with the CSV), so there is no
// /jobs/:id/results route.
import type { RouteDef } from '../../router.tsx'
import { JobDetailPage } from './JobDetailPage.tsx'
import { JobsPage } from './JobsPage.tsx'

export const routes: RouteDef[] = [
  { path: '/jobs', title: 'jobs.title', element: JobsPage, protected: true },
  { path: '/jobs/:id', title: 'jobs.detail.title', element: JobDetailPage, protected: true },
]

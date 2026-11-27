// The routes of the 'newjob' area. This file is filled by the screen agent of the area; src/routes.ts only imports it.
//
//   /new/verify   a new verification job (and `?job=<id>` continues a draft)
//   /new/send     a new send job, in four steps (and `?job=<id>` continues a draft)
//
// The navigation (src/shell/navItems.ts) links to both; the jobs page links a draft to its own page with `?job=`.
import type { RouteDef } from '../../router.tsx'
import { NewSendPage } from './NewSendPage.tsx'
import { NewVerifyPage } from './NewVerifyPage.tsx'

export const routes: RouteDef[] = [
  { path: '/new/verify', title: 'newjob.verify.title', element: NewVerifyPage, protected: true },
  { path: '/new/send', title: 'newjob.send.title', element: NewSendPage, protected: true },
]

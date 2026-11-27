// The sign-in pages. Mine (the foundation): the area agents never touch this file. None of them is `protected`; the guard (auth/guard.ts) keeps them apart:
//   sign in, forgot password   guestOnly   (a signed-in user goes on to the dashboard)
//   accept invite, reset       passwordSetup (only with the session of an invitation or reset link; anyone else goes to sign in)
import { ACCEPT_INVITE_PATH, FORGOT_PASSWORD_PATH, RESET_PASSWORD_PATH, SIGN_IN_PATH } from '../../auth/paths.ts'
import type { RouteDef } from '../../router.tsx'
import { ForgotPasswordPage } from './ForgotPasswordPage.tsx'
import { AcceptInvitePage, ResetPasswordPage } from './SetPasswordPage.tsx'
import { SignInPage } from './SignInPage.tsx'

export const routes: RouteDef[] = [
  { path: SIGN_IN_PATH, title: 'auth.signIn.title', element: SignInPage, protected: false, guestOnly: true },
  { path: FORGOT_PASSWORD_PATH, title: 'auth.forgot.title', element: ForgotPasswordPage, protected: false, guestOnly: true },
  { path: ACCEPT_INVITE_PATH, title: 'auth.invite.title', element: AcceptInvitePage, protected: false, passwordSetup: true },
  { path: RESET_PASSWORD_PATH, title: 'auth.reset.title', element: ResetPasswordPage, protected: false, passwordSetup: true },
]

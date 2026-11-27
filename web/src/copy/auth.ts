// Copy of the sign-in area: sign in, forgot password, and the page that sets a password from an invitation or a reset link.
// Every sign-in service failure becomes one of the 'auth.error.*' lines (src/auth/errors.ts); the service's own text is never shown.
import type { AreaCopy } from './types.ts'

export const auth = {
  'auth.signIn.title': 'Sign in',
  'auth.signIn.intro': 'Use the email address and password of your account.',
  'auth.field.email': 'Email address',
  'auth.field.password': 'Password',
  'auth.signIn.submit': 'Sign in',
  'auth.signIn.missing': 'Enter your email address and your password.',
  'auth.signIn.working': 'Signing in',
  'auth.signIn.forgot': 'Forgot your password?',
  'auth.signIn.noAccount': 'Accounts are created by invitation. If you need one, ask the person who runs your account.',

  'auth.forgot.title': 'Reset your password',
  'auth.forgot.intro': 'Enter the email address of your account and we will send you a link to choose a new password.',
  'auth.forgot.missing': 'Enter the email address of your account.',
  'auth.forgot.submit': 'Send the reset link',
  'auth.forgot.working': 'Sending',
  'auth.forgot.sent':
    'If that address has an account, we have sent a link to reset its password. It can take a few minutes to arrive. Check your spam folder too.',
  'auth.forgot.back': 'Back to sign in',

  'auth.invite.title': 'Welcome. Choose your password',
  'auth.invite.intro': 'Choose a password to finish setting up your account.',
  'auth.reset.title': 'Choose a new password',
  'auth.reset.intro': 'Choose a new password for your account.',
  'auth.password.new': 'New password',
  'auth.password.confirm': 'Confirm the new password',
  'auth.password.rule':
    'Use {min} to {max} characters. A phrase of several words is easy to remember and hard to guess.',
  'auth.password.tooShort': 'The password needs at least {min} characters.',
  'auth.password.tooLong': 'The password can be at most {max} characters.',
  'auth.password.mismatch': 'The two passwords do not match.',
  'auth.password.sameAsEmail': 'The password cannot be your email address.',
  'auth.password.submit': 'Save the password and continue',
  'auth.password.working': 'Saving',

  'auth.notice.sessionEnded': 'Your session ended. Sign in again to continue.',
  'auth.notice.linkExpired':
    'That link has expired or was already used. Sign in, or choose Forgot your password to get a new link.',

  'auth.error.invalid_credentials': 'That email address and password do not match. Check them and try again.',
  'auth.error.email_not_confirmed':
    'This account is not active yet. Open the invitation email we sent you, or ask for a new one.',
  'auth.error.account_disabled': 'This account cannot sign in. Contact support.',
  'auth.error.rate_limited': 'Too many attempts. Wait a few minutes and try again.',
  'auth.error.weak_password': 'That password is too easy to guess. Choose a longer one that you do not use anywhere else.',
  'auth.error.same_password': 'Choose a password that is different from your current one.',
  'auth.error.session_ended': 'This link has expired or was already used. Ask for a new one with Forgot your password.',
  'auth.error.unavailable': 'The sign-in service is not answering. Try again in a minute.',
  'auth.error.network': 'We could not reach the sign-in service. Check your connection and try again.',
  'auth.error.unknown': 'We could not sign you in. Try again in a moment.',
} as const satisfies AreaCopy<'auth'>

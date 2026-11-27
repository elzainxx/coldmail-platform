// The rule for a new password, stated on the page (copy: auth.password.rule). Length only, as current guidance has it: no composition rules, and a phrase of
// several words is welcome. The service (GoTrue) may be stricter; its answer becomes the 'weak_password' line.

export const PASSWORD_MIN_LENGTH = 12
/** GoTrue hashes with bcrypt, which reads at most 72 bytes. */
export const PASSWORD_MAX_BYTES = 72

export type PasswordProblem = 'tooShort' | 'tooLong' | 'sameAsEmail' | 'mismatch'

/** The first thing wrong with a new password, or null. `email` is optional (the address is not known on every page). */
export function passwordProblem(password: string, confirmation: string, email?: string): PasswordProblem | null {
  if ([...password].length < PASSWORD_MIN_LENGTH) return 'tooShort'
  if (new TextEncoder().encode(password).length > PASSWORD_MAX_BYTES) return 'tooLong'
  if (email !== undefined && password.trim().toLowerCase() === email.trim().toLowerCase()) return 'sameAsEmail'
  if (password !== confirmation) return 'mismatch'
  return null
}

import { useState, type FormEvent } from 'react'
import { authErrorKeyOf } from '../../auth/errors.ts'
import { PASSWORD_MAX_BYTES, PASSWORD_MIN_LENGTH, passwordProblem, type PasswordProblem } from '../../auth/password.ts'
import { session } from '../../auth/session.ts'
import { t, type CopyKey } from '../../copy/index.ts'
import { Button, Card, FormField, PageHeader, TextInput } from '../../ui/index.ts'

const PROBLEM_COPY: Record<PasswordProblem, CopyKey> = {
  tooShort: 'auth.password.tooShort',
  tooLong: 'auth.password.tooLong',
  sameAsEmail: 'auth.password.sameAsEmail',
  mismatch: 'auth.password.mismatch',
}

type Mode = 'invite' | 'recovery'

/**
 * Choose a password after opening an invitation (mode "invite") or a reset link (mode "recovery"). The session behind it came from the link (kept in memory
 * only, the address bar was cleaned at start); until a password is set the rest of the app is closed. The rule is stated above the field.
 */
function SetPasswordPage({ mode }: { mode: Mode }) {
  const [password, setPassword] = useState('')
  const [confirmation, setConfirmation] = useState('')
  const [problem, setProblem] = useState<PasswordProblem | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    const found = passwordProblem(password, confirmation)
    setProblem(found)
    setError(null)
    if (found !== null) return
    setBusy(true)
    try {
      // On success the session becomes a signed-in one and the router moves to the dashboard (auth/guard.ts).
      await session.setPassword(password)
    } catch (err) {
      setError(t(authErrorKeyOf(err)))
      setBusy(false)
    }
  }

  const rule = t('auth.password.rule', { min: PASSWORD_MIN_LENGTH, max: PASSWORD_MAX_BYTES })
  const text = (key: CopyKey) => t(key, { min: PASSWORD_MIN_LENGTH, max: PASSWORD_MAX_BYTES })
  const passwordError = problem !== null && problem !== 'mismatch' ? text(PROBLEM_COPY[problem]) : null
  const confirmError = problem === 'mismatch' ? text(PROBLEM_COPY.mismatch) : null

  return (
    <section>
      <PageHeader title={mode === 'invite' ? t('auth.invite.title') : t('auth.reset.title')} description={mode === 'invite' ? t('auth.invite.intro') : t('auth.reset.intro')} />
      <Card>
        <form onSubmit={submit} noValidate className="flex flex-col gap-3">
          {error !== null && (
            <p role="alert" className="rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
              {error}
            </p>
          )}
          <FormField label={t('auth.password.new')} hint={rule} error={passwordError} required>
            <TextInput type="password" name="new-password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} />
          </FormField>
          <FormField label={t('auth.password.confirm')} error={confirmError} required>
            <TextInput type="password" name="confirm-password" autoComplete="new-password" value={confirmation} onChange={(e) => setConfirmation(e.target.value)} />
          </FormField>
          <Button type="submit" variant="primary" busy={busy}>
            {busy ? t('auth.password.working') : t('auth.password.submit')}
          </Button>
        </form>
      </Card>
    </section>
  )
}

export function AcceptInvitePage() {
  return <SetPasswordPage mode="invite" />
}

export function ResetPasswordPage() {
  return <SetPasswordPage mode="recovery" />
}

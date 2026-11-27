import { useState, type FormEvent } from 'react'
import { authErrorKeyOf } from '../../auth/errors.ts'
import { FORGOT_PASSWORD_PATH } from '../../auth/paths.ts'
import { session } from '../../auth/session.ts'
import { useSession } from '../../auth/useSession.ts'
import { t } from '../../copy/index.ts'
import { Link, useRoute } from '../../router.tsx'
import { Button, Card, FormField, PageHeader, TextInput } from '../../ui/index.ts'

/** Sign in with an email address and a password. There is no sign-up here and no link to one: accounts are made by invitation. */
export function SignInPage() {
  const { query } = useRoute()
  const { endedBecause } = useSession()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const notice =
    endedBecause === 'expired' ? t('auth.notice.sessionEnded') : query.get('notice') === 'link_expired' ? t('auth.notice.linkExpired') : null

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    if (email.trim() === '' || password === '') {
      setError(t('auth.signIn.missing'))
      return
    }
    setError(null)
    setBusy(true)
    try {
      // On success the session changes and the router sends the user where they were going (auth/guard.ts); nothing else to do here.
      await session.signIn(email.trim(), password)
    } catch (err) {
      setPassword('')
      setError(t(authErrorKeyOf(err)))
      setBusy(false)
    }
  }

  return (
    <section>
      <PageHeader title={t('auth.signIn.title')} description={t('auth.signIn.intro')} />
      {notice !== null && (
        <p role="status" className="mb-3 rounded-md bg-status-risky-bg px-3 py-2 text-sm text-status-risky">
          {notice}
        </p>
      )}
      <Card>
        <form onSubmit={submit} noValidate className="flex flex-col gap-3">
          {error !== null && (
            <p role="alert" className="rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
              {error}
            </p>
          )}
          <FormField label={t('auth.field.email')} required>
            <TextInput type="email" name="email" autoComplete="username" autoCapitalize="none" spellCheck={false} value={email} onChange={(e) => setEmail(e.target.value)} />
          </FormField>
          <FormField label={t('auth.field.password')} required>
            <TextInput type="password" name="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
          </FormField>
          <Button type="submit" variant="primary" busy={busy}>
            {busy ? t('auth.signIn.working') : t('auth.signIn.submit')}
          </Button>
        </form>
      </Card>
      <p className="mt-3 text-sm">
        <Link to={FORGOT_PASSWORD_PATH}>{t('auth.signIn.forgot')}</Link>
      </p>
      <p className="mt-2 text-xs text-fg-muted">{t('auth.signIn.noAccount')}</p>
    </section>
  )
}

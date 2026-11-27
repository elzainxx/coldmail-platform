import { useState, type FormEvent } from 'react'
import { authErrorKeyOf } from '../../auth/errors.ts'
import { SIGN_IN_PATH } from '../../auth/paths.ts'
import { session } from '../../auth/session.ts'
import { t } from '../../copy/index.ts'
import { Link } from '../../router.tsx'
import { Button, Card, FormField, PageHeader, TextInput } from '../../ui/index.ts'

/**
 * Ask for a reset link. The answer is the SAME sentence whether or not the address has an account (so the page cannot be used to find out who has one);
 * only a request that could not be made at all is reported.
 */
export function ForgotPasswordPage() {
  const [email, setEmail] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [sent, setSent] = useState(false)
  const [busy, setBusy] = useState(false)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    if (email.trim() === '') {
      setError(t('auth.forgot.missing'))
      return
    }
    setError(null)
    setBusy(true)
    try {
      await session.recover(email.trim())
      setSent(true)
    } catch (err) {
      setError(t(authErrorKeyOf(err)))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section>
      <PageHeader title={t('auth.forgot.title')} description={sent ? undefined : t('auth.forgot.intro')} />
      <Card>
        {sent ? (
          <p role="status">{t('auth.forgot.sent')}</p>
        ) : (
          <form onSubmit={submit} noValidate className="flex flex-col gap-3">
            <FormField label={t('auth.field.email')} error={error} required>
              <TextInput type="email" name="email" autoComplete="username" autoCapitalize="none" spellCheck={false} value={email} onChange={(e) => setEmail(e.target.value)} />
            </FormField>
            <Button type="submit" variant="primary" busy={busy}>
              {busy ? t('auth.forgot.working') : t('auth.forgot.submit')}
            </Button>
          </form>
        )}
      </Card>
      <p className="mt-3 text-sm">
        <Link to={SIGN_IN_PATH}>{t('auth.forgot.back')}</Link>
      </p>
    </section>
  )
}

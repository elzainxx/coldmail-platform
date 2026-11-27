// Settings: the vendor opt-out (with its disclosure), the account (the address, change the password) and sign out.
//
// THE PASSWORDS of the change-password form are in uncontrolled inputs: no state holds them, they are read from the form at the submit, handed to the session's
// sign-in client and emptied. They are in no address and in no log.
import { useEffect, useState, type FormEvent } from 'react'
import { meApi, settingsApi } from '../../api/endpoints.ts'
import { isAbortError, isApiError } from '../../api/errors.ts'
import { isAuthError, type AuthErrorKind } from '../../auth/gotrue.ts'
import { SIGN_IN_PATH } from '../../auth/paths.ts'
import { PASSWORD_MAX_BYTES, PASSWORD_MIN_LENGTH, passwordProblem, type PasswordProblem } from '../../auth/password.ts'
import { session } from '../../auth/session.ts'
import { errorReference, errorText, t, type CopyKey } from '../../copy/index.ts'
import { navigate } from '../../router.tsx'
import { Button, Card, Checkbox, FormField, PageHeader, Spinner, TextInput, useToast } from '../../ui/index.ts'

interface Failure {
  text: string
  reference: string | null
}

function failureOf(err: unknown): Failure {
  // A thrown value that is not an ApiError is a mistake of the app's own: the user gets the generic line.
  if (!isApiError(err)) return { text: t('error.unknown'), reference: null }
  return { text: errorText(err), reference: errorReference(err) }
}

function FailureNote({ failure }: { failure: Failure }) {
  return (
    <div role="alert" className="rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
      <p>{failure.text}</p>
      {failure.reference !== null && <p className="mt-1 text-xs">{failure.reference}</p>}
    </div>
  )
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// The vendor opt-out
// ---------------------------------------------------------------------------------------------------------------------------------------------------

function VendorCard() {
  const toast = useToast()
  /** what the service says the setting is now (null until it has said) */
  const [optOut, setOptOut] = useState<boolean | null>(null)
  const [loadFailure, setLoadFailure] = useState<Failure | null>(null)
  const [saving, setSaving] = useState(false)
  const [saveFailure, setSaveFailure] = useState<Failure | null>(null)
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    const controller = new AbortController()
    setLoadFailure(null)
    settingsApi.get({ signal: controller.signal }).then(
      (s) => setOptOut(s.vendor_fallback_opt_out),
      (err: unknown) => {
        if (!isAbortError(err) && !controller.signal.aborted) setLoadFailure(failureOf(err))
      },
    )
    return () => controller.abort()
  }, [attempt])

  // The box shows what the service holds: it changes when the service has answered, not before, so a refusal leaves it where it was.
  const change = async (next: boolean) => {
    if (saving) return
    setSaving(true)
    setSaveFailure(null)
    try {
      const now = await settingsApi.put({ vendor_fallback_opt_out: next })
      setOptOut(now.vendor_fallback_opt_out)
      toast.notify(t(now.vendor_fallback_opt_out ? 'settings.vendor.savedOn' : 'settings.vendor.savedOff'), { tone: 'success' })
    } catch (err) {
      if (isAbortError(err)) return
      setSaveFailure(failureOf(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card title={t('settings.vendor.title')}>
      <div className="flex max-w-2xl flex-col gap-3">
        {loadFailure !== null && (
          <>
            <FailureNote failure={loadFailure} />
            <div>
              <Button onClick={() => setAttempt((n) => n + 1)}>{t('common.retry')}</Button>
            </div>
          </>
        )}
        {loadFailure === null && optOut === null && <Spinner label={t('common.loading')} />}
        {optOut !== null && (
          <>
            {saveFailure !== null && (
              <>
                <p className="text-base text-fg">{t('settings.vendor.failed')}</p>
                <FailureNote failure={saveFailure} />
              </>
            )}
            <Checkbox
              label={t('settings.vendor.label')}
              description={t('settings.disclosure.vendor')}
              checked={optOut}
              aria-busy={saving || undefined}
              onChange={(e) => void change(e.target.checked)}
            />
            <p className="text-sm text-fg-muted">{t('settings.vendor.scope')}</p>
            {saving && (
              <p role="status" className="text-sm text-fg-muted">
                {t('settings.vendor.saving')}
              </p>
            )}
          </>
        )}
      </div>
    </Card>
  )
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// The account: the address and the password
// ---------------------------------------------------------------------------------------------------------------------------------------------------

const PROBLEM_COPY: Record<PasswordProblem, CopyKey> = {
  tooShort: 'auth.password.tooShort',
  tooLong: 'auth.password.tooLong',
  sameAsEmail: 'auth.password.sameAsEmail',
  mismatch: 'auth.password.mismatch',
}

/** What each way the sign-in service can refuse a new password means to the user (its own text is never read). */
const AUTH_LINE: Partial<Record<AuthErrorKind, CopyKey>> = {
  weak_password: 'auth.error.weak_password',
  same_password: 'auth.error.same_password',
  rate_limited: 'auth.error.rate_limited',
  unavailable: 'auth.error.unavailable',
  network: 'auth.error.network',
  session_ended: 'settings.password.sessionEnded',
}

function passwordFailureText(err: unknown): string {
  const key = isAuthError(err) ? AUTH_LINE[err.kind] : undefined
  return t(key ?? 'settings.password.failed')
}

function AccountCard() {
  const toast = useToast()
  const [email, setEmail] = useState<string | null>(null)
  const [emailFailed, setEmailFailed] = useState(false)
  const [busy, setBusy] = useState(false)
  const [problem, setProblem] = useState<PasswordProblem | null>(null)
  const [failure, setFailure] = useState<string | null>(null)

  useEffect(() => {
    const controller = new AbortController()
    meApi.get({ signal: controller.signal }).then(
      (me) => setEmail(me.email),
      (err: unknown) => {
        if (!isAbortError(err) && !controller.signal.aborted) setEmailFailed(true)
      },
    )
    return () => controller.abort()
  }, [])

  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    if (busy) return
    const form = e.currentTarget
    const data = new FormData(form)
    const read = (name: string): string => {
      const v = data.get(name)
      return typeof v === 'string' ? v : ''
    }
    const password = read('new-password')
    const found = passwordProblem(password, read('confirm-password'), email ?? undefined)
    setProblem(found)
    setFailure(null)
    if (found !== null) return // nothing was sent: what the user typed stays
    setBusy(true)
    try {
      await session.setPassword(password)
      toast.notify(t('settings.password.done'), { tone: 'success' })
    } catch (err) {
      if (isAbortError(err)) return
      setFailure(passwordFailureText(err))
    } finally {
      // sent, whatever came of it: neither password stays in the page
      form.reset()
      setBusy(false)
    }
  }

  const text = (key: CopyKey) => t(key, { min: PASSWORD_MIN_LENGTH, max: PASSWORD_MAX_BYTES })
  const newError = problem !== null && problem !== 'mismatch' ? text(PROBLEM_COPY[problem]) : null
  const confirmError = problem === 'mismatch' ? text(PROBLEM_COPY.mismatch) : null

  return (
    <Card title={t('settings.account.title')}>
      <div className="flex max-w-xl flex-col gap-4">
        <dl className="flex flex-col gap-0.5">
          <dt className="text-sm font-medium text-fg">{t('settings.account.email')}</dt>
          <dd className="text-base break-all text-fg">{email ?? (emailFailed ? t('settings.account.emailUnknown') : <Spinner size={14} label={t('common.loading')} />)}</dd>
        </dl>

        <form method="post" onSubmit={(e) => void submit(e)} noValidate className="flex flex-col gap-3" aria-labelledby="change-password-title">
          <h3 id="change-password-title" className="text-base font-semibold text-fg">
            {t('settings.password.title')}
          </h3>
          {failure !== null && (
            <p role="alert" className="rounded-md bg-status-invalid-bg px-3 py-2 text-sm text-status-invalid">
              {failure}
            </p>
          )}
          <FormField label={t('auth.password.new')} hint={text('auth.password.rule')} error={newError} required>
            <TextInput type="password" name="new-password" autoComplete="new-password" />
          </FormField>
          <FormField label={t('auth.password.confirm')} error={confirmError} required>
            <TextInput type="password" name="confirm-password" autoComplete="new-password" />
          </FormField>
          {busy && (
            <p role="status" className="text-sm text-fg-muted">
              {t('settings.password.working')}
            </p>
          )}
          <div>
            <Button type="submit" variant="primary" busy={busy}>
              {t('settings.password.submit')}
            </Button>
          </div>
        </form>
      </div>
    </Card>
  )
}

// ---------------------------------------------------------------------------------------------------------------------------------------------------
// Sign out
// ---------------------------------------------------------------------------------------------------------------------------------------------------

function SessionCard() {
  const signOut = () => {
    void session.signOut() // wipes the tokens at once (GoTrue is told afterwards) ...
    navigate(SIGN_IN_PATH, { replace: true }) // ... so the sign-in page is shown with nothing of the user left
  }
  return (
    <Card title={t('settings.signOut.title')}>
      <div className="flex max-w-xl flex-col items-start gap-3">
        <p className="text-base text-fg-muted">{t('settings.signOut.body')}</p>
        <Button onClick={signOut}>{t('common.signOut')}</Button>
      </div>
    </Card>
  )
}

export function SettingsPage() {
  return (
    <section className="flex flex-col gap-4">
      <PageHeader title={t('settings.title')} />
      <VendorCard />
      <AccountCard />
      <SessionCard />
    </section>
  )
}

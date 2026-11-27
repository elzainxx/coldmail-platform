// The form that adds one SMTP account.
//
// THE PASSWORD. It is in the input and nowhere else: the inputs are not controlled, so no state of this component (or of any other) ever holds it; it is read from the
// form at the moment of the submit, goes in the body of the one request (never a URL: the form is `method="post"` and the submit is handled, so even a submit that
// escaped the handler would not put it in an address), and the input is emptied as soon as the answer is in. The one case it stays is the answer "confirm the
// warning first": the same account is sent again as it was, with the box ticked, so the user is not asked to type it twice. It is emptied when the form is closed too.
import { useEffect, useRef, useState, type FormEvent } from 'react'
import { m3Api } from '../../api/endpoints.ts'
import { isAbortError, isApiError } from '../../api/errors.ts'
import type { M3Account } from '../../api/types.ts'
import { errorText, t } from '../../copy/index.ts'
import { fieldIssueText, fieldIssuesOf } from '../../copy/words.ts'
import { Button, Card, Checkbox, FormField, TextInput } from '../../ui/index.ts'
import { isAccountField, parseAccountForm, type FieldErrors } from './accountForm.ts'
import { ErrorNote } from './ErrorNote.tsx'
import { failureOf, type Failure } from './words.ts'

export interface AddAccountFormProps {
  /** the account was added (whatever its login check and its From address check said: they are on the account) */
  onAdded: (account: M3Account) => void
  onCancel: () => void
  /** the answer was lost: the account may exist, so the list must be read again */
  onOutcomeUnknown: () => void
}

interface Ack {
  /** the API wants the warning confirmed before it adds the account */
  needed: boolean
  checked: boolean
  /** the user pressed the button without ticking the box */
  missing: boolean
}

const NO_ACK: Ack = { needed: false, checked: false, missing: false }

function clearSecret(form: HTMLFormElement): void {
  const input = form.elements.namedItem('smtp_secret')
  if (input instanceof HTMLInputElement) input.value = ''
}

export function AddAccountForm({ onAdded, onCancel, onOutcomeUnknown }: AddAccountFormProps) {
  const formRef = useRef<HTMLFormElement>(null)
  const ackRef = useRef<HTMLDivElement>(null)
  const [busy, setBusy] = useState(false)
  const [errors, setErrors] = useState<FieldErrors>({})
  const [failure, setFailure] = useState<Failure | null>(null)
  const [ack, setAck] = useState<Ack>(NO_ACK)
  const [focusInvalid, setFocusInvalid] = useState(0)

  // after a submit that found a mistake, the cursor goes to the first field that has one
  useEffect(() => {
    if (focusInvalid > 0) formRef.current?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus()
  }, [focusInvalid])

  // the warning appears under the form: take the user to its box
  useEffect(() => {
    if (ack.needed) ackRef.current?.querySelector<HTMLElement>('input')?.focus()
  }, [ack.needed])

  // closed with a request still out: its answer belongs to nobody
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    const form = formRef.current
    return () => {
      mounted.current = false
      if (form !== null) clearSecret(form)
    }
  }, [])

  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    if (busy) return
    const form = e.currentTarget
    setFailure(null)

    const parsed = parseAccountForm(new FormData(form), ack.needed && ack.checked)
    setErrors(parsed.errors)
    if (parsed.account === null) {
      setFocusInvalid((n) => n + 1)
      return // nothing was sent: the password stays where the user typed it
    }
    if (ack.needed && !ack.checked) {
      setAck({ ...ack, missing: true })
      ackRef.current?.querySelector<HTMLElement>('input')?.focus()
      return
    }

    setBusy(true)
    try {
      const res = await m3Api.add(parsed.account)
      clearSecret(form)
      form.reset()
      if (mounted.current) onAdded(res.account)
    } catch (err) {
      if (isAbortError(err) || !mounted.current) return
      if (isApiError(err) && err.code === 'from_gate_ack_required') {
        // not an error: the account is held until the warning is confirmed. The password stays so that the same account can be sent again.
        setAck({ needed: true, checked: false, missing: false })
        return
      }
      clearSecret(form)
      setAck(NO_ACK)
      if (isApiError(err) && err.code === 'validation_failed') {
        const marked: FieldErrors = {}
        for (const issue of fieldIssuesOf(err)) {
          if (isAccountField(issue.field)) marked[issue.field] = fieldIssueText(issue.issue)
        }
        if (Object.keys(marked).length > 0) {
          setErrors(marked)
          setFocusInvalid((n) => n + 1)
          setFailure({ text: errorText(err), wait: null, reference: null })
          return
        }
      }
      if (isApiError(err) && err.code === 'host_forbidden') {
        setErrors({ smtp_host: errorText(err) })
        setFocusInvalid((n) => n + 1)
        return
      }
      setFailure(failureOf(err))
      if (isApiError(err) && err.code === 'account_outcome_unknown') onOutcomeUnknown()
    } finally {
      if (mounted.current) setBusy(false)
    }
  }

  const cancel = () => {
    if (formRef.current !== null) clearSecret(formRef.current)
    onCancel()
  }

  // a change to the account is a different account: a warning confirmed for the old address does not cover it
  const edited = (e: FormEvent<HTMLFormElement>) => {
    if ((e.target as HTMLInputElement).name === 'acknowledge') return
    if (ack.needed) setAck(NO_ACK)
  }

  return (
    <Card title={t('accounts.add.title')}>
      <form ref={formRef} method="post" onSubmit={(e) => void submit(e)} onChange={edited} noValidate autoComplete="off" className="flex max-w-xl flex-col gap-3">
        <p className="text-base text-fg-muted">{t('accounts.add.intro')}</p>
        {failure !== null && <ErrorNote failure={failure} />}

        <FormField label={t('accounts.add.host')} hint={t('accounts.add.hostHint')} error={errors.smtp_host} required>
          <TextInput name="smtp_host" autoComplete="off" autoCapitalize="none" spellCheck={false} autoFocus />
        </FormField>
        <FormField label={t('accounts.add.port')} hint={t('accounts.add.portHint')} error={errors.smtp_port} optional>
          <TextInput name="smtp_port" inputMode="numeric" autoComplete="off" className="max-w-32" />
        </FormField>
        <FormField label={t('accounts.add.username')} error={errors.smtp_username} required>
          <TextInput name="smtp_username" autoComplete="off" autoCapitalize="none" spellCheck={false} />
        </FormField>
        <FormField label={t('accounts.add.password')} hint={t('accounts.add.passwordHint')} error={errors.smtp_secret} required>
          <TextInput name="smtp_secret" type="password" autoComplete="new-password" />
        </FormField>
        <FormField label={t('accounts.add.fromAddress')} hint={t('accounts.add.fromAddressHint')} error={errors.from_address} required>
          <TextInput name="from_address" type="email" autoComplete="off" autoCapitalize="none" spellCheck={false} />
        </FormField>
        <FormField label={t('accounts.add.fromName')} hint={t('accounts.add.fromNameHint')} error={errors.from_name} optional>
          <TextInput name="from_name" autoComplete="off" />
        </FormField>
        <FormField label={t('accounts.add.dailyLimit')} hint={t('accounts.add.dailyLimitHint')} error={errors.daily_limit} optional>
          <TextInput name="daily_limit" inputMode="numeric" autoComplete="off" className="max-w-32" />
        </FormField>

        {ack.needed && (
          <div ref={ackRef} className="flex flex-col gap-2 rounded-md bg-status-risky-bg px-3 py-3 text-status-risky">
            <p className="text-base font-medium">{t('accounts.ack.title')}</p>
            <p className="text-base">{t('accounts.ack.body')}</p>
            <Checkbox
              name="acknowledge"
              label={t('accounts.ack.checkbox')}
              checked={ack.checked}
              aria-invalid={ack.missing && !ack.checked ? true : undefined}
              onChange={(e) => setAck({ needed: true, checked: e.target.checked, missing: false })}
            />
            {ack.missing && !ack.checked && (
              <p role="alert" className="text-sm font-medium">
                {t('accounts.ack.needed')}
              </p>
            )}
          </div>
        )}

        {busy && (
          <p role="status" className="text-sm text-fg-muted">
            {t('accounts.add.working')}
          </p>
        )}
        <div className="flex flex-wrap gap-2">
          <Button type="submit" variant="primary" busy={busy}>
            {ack.needed ? t('accounts.add.submitAnyway') : t('accounts.add.submit')}
          </Button>
          <Button onClick={cancel} disabled={busy}>
            {t('common.cancel')}
          </Button>
        </div>
      </form>
    </Card>
  )
}

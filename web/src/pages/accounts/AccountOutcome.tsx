import { useEffect, useRef } from 'react'
import type { M3Account } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { Button, Card, StatusBadge } from '../../ui/index.ts'
import { fromGateLabel, fromGateToken, fromGateWhy, verifyWhy } from './words.ts'

/** What an add came to, in words: whether the login works, and what the check of the From address says. The account was added whatever these say. */
export function AccountOutcome({ account, onDismiss }: { account: M3Account; onDismiss: () => void }) {
  const ref = useRef<HTMLDivElement>(null)
  // the user's eyes are on the form that just went away: put focus where the result is, so it is read
  useEffect(() => {
    ref.current?.focus()
  }, [])

  const login =
    account.verification === 'verified'
      ? t('accounts.verification.verified')
      : account.verification === 'failed'
        ? t('accounts.added.loginFailed', { why: verifyWhy(account.verify_code) })
        : t('accounts.added.loginUnchecked')

  return (
    <div ref={ref} tabIndex={-1} className="outline-offset-4">
      <Card
        title={t('accounts.added.title', { address: account.from_address })}
        actions={
          <Button size="sm" onClick={onDismiss}>
            {t('accounts.added.dismiss')}
          </Button>
        }
      >
        <div role="status" className="flex flex-col gap-2">
          <p className="text-base text-fg">{login}</p>
          <p className="flex flex-wrap items-center gap-2 text-base text-fg">
            <StatusBadge status={fromGateToken(account.from_gate)} label={fromGateLabel(account.from_gate)} />
            <span>{fromGateWhy(account.from_gate)}</span>
          </p>
        </div>
      </Card>
    </div>
  )
}

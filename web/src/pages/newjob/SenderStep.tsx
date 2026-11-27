// Step 1 of a send: who sends it. "Managed sending" (our servers, paid in credits) or "Your own SMTP" (the user's own mailboxes, under an entitlement). Each says
// what the user needs for it and whether they have it: credits for the first, the entitlement and a sender account for the second, with a link to the accounts
// page when something is missing. It says; it does not forbid: the platform refuses a confirm that cannot be done, and the user may be preparing a message first.
import type { ReactNode } from 'react'
import type { Balances } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { Link } from '../../router.tsx'
import { Choice, ChoiceGroup } from './Choice.tsx'
import { fmt, formatWhen } from './schedule.ts'
import type { AccountsFact } from './useSenderFacts.ts'

export type Sender = 'm2' | 'm3'

function Note({ children }: { children: ReactNode }) {
  return (
    <p role="status" className="rounded-md bg-sunken px-3 py-2 text-sm text-fg">
      {children}
    </p>
  )
}

function ManagedFacts({ balances }: { balances: Balances | null }) {
  if (balances === null) return <Note>{t('newjob.sender.managedPlain')}</Note>
  return <Note>{t('newjob.sender.managedCredits', { credits: fmt(balances.modules.m2.available) })}</Note>
}

function OwnFacts({ balances, accounts }: { balances: Balances | null; accounts: AccountsFact }) {
  const lines: ReactNode[] = []
  const m3 = balances?.m3
  if (m3 !== undefined) {
    if (m3.state === 'suspended') lines.push(<>{t('newjob.entitlement.suspended')}</>)
    else if (!m3.enabled) lines.push(<>{t('newjob.entitlement.off')}</>)
    else if (m3.until !== null) lines.push(<>{t('newjob.entitlement.until', { date: formatWhen(m3.until) })}</>)
    else lines.push(<>{t('newjob.entitlement.on')}</>)
  }
  if (accounts.status === 'ready') {
    lines.push(accounts.usable === 0 ? <>{t('newjob.sender.noAccount')}</> : <>{t('newjob.sender.accountsReady', { count: fmt(accounts.usable) })}</>)
  }
  const trouble = (m3 !== undefined && (m3.state === 'suspended' || !m3.enabled)) || (accounts.status === 'ready' && accounts.usable === 0)
  return (
    <Note>
      {t('newjob.sender.ownPlain')}
      {lines.map((line, i) => (
        <span key={i} className="mt-1 block">
          {line}
        </span>
      ))}
      {trouble && (
        <span className="mt-1 block">
          <Link to="/accounts">{t('newjob.sender.accountsLink')}</Link>
        </span>
      )}
    </Note>
  )
}

export interface SenderStepProps {
  sender: Sender | null
  onSender: (s: Sender) => void
  balances: Balances | null
  accounts: AccountsFact
}

export function SenderStep({ sender, onSender, balances, accounts }: SenderStepProps) {
  return (
    <div className="flex flex-col gap-3">
      <ChoiceGroup legend={t('newjob.sender.legend')}>
        <Choice
          name="sender"
          value="m2"
          checked={sender === 'm2'}
          onChange={() => onSender('m2')}
          label={t('newjob.sender.managed')}
          description={t('newjob.sender.managedDescription')}
        />
        <Choice
          name="sender"
          value="m3"
          checked={sender === 'm3'}
          onChange={() => onSender('m3')}
          label={t('newjob.sender.own')}
          description={t('newjob.sender.ownDescription')}
        />
      </ChoiceGroup>
      {sender === 'm2' && <ManagedFacts balances={balances} />}
      {sender === 'm3' && <OwnFacts balances={balances} accounts={accounts} />}
    </div>
  )
}

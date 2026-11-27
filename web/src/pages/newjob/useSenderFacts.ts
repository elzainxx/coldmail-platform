// What the sender step needs to know about the user to say anything useful: their credits and entitlement (GET /v1/balances), and for "Your own SMTP" whether
// they have a sender account that can be sent from. Both are read once; a failed read is not an error the user must deal with (the step then says less).
import { useEffect, useState } from 'react'
import { balancesApi, m3Api } from '../../api/endpoints.ts'
import type { Balances, M3Account } from '../../api/types.ts'

export function useBalances(): Balances | null {
  const [balances, setBalances] = useState<Balances | null>(null)
  useEffect(() => {
    const c = new AbortController()
    balancesApi.get({ signal: c.signal }).then(
      (b) => {
        if (!c.signal.aborted) setBalances(b)
      },
      () => undefined,
    )
    return () => c.abort()
  }, [])
  return balances
}

/** An account a campaign can be sent from: switched on, its login not refused, and its From address not blocked. A warned or unchecked one still counts. */
export function accountUsable(a: M3Account): boolean {
  return a.status === 'active' && a.verification !== 'failed' && a.from_gate !== 'blocked'
}

export type AccountsFact = { status: 'idle' | 'loading' | 'failed' } | { status: 'ready'; usable: number; total: number }

/** The user's module 3 accounts, once `enabled` (the user chose "Your own SMTP"). Only the first page is read: it is enough to know whether one can send. */
export function useAccountsFact(enabled: boolean): AccountsFact {
  const [fact, setFact] = useState<AccountsFact>({ status: 'idle' })
  useEffect(() => {
    if (!enabled) return undefined
    const c = new AbortController()
    setFact({ status: 'loading' })
    m3Api.list({}, { signal: c.signal }).then(
      (page) => {
        if (!c.signal.aborted) setFact({ status: 'ready', usable: page.accounts.filter(accountUsable).length, total: page.accounts.length })
      },
      () => {
        if (!c.signal.aborted) setFact({ status: 'failed' })
      },
    )
    return () => c.abort()
  }, [enabled])
  return fact
}

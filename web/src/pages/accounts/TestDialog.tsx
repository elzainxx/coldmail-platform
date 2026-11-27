// The Test button's answer: how far the connection got, in words, and what the check of the From address says. A warning has to be confirmed before the account is
// used, and the confirmation travels with the next test (the same rule as when the account is added): the button that sends it stays off until the box is ticked.
import { useState } from 'react'
import type { M3Account, M3TestResponse } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { Button, Checkbox, Dialog, Spinner, StatusBadge } from '../../ui/index.ts'
import { ErrorNote } from './ErrorNote.tsx'
import { fromGateLabel, fromGateToken, fromGateWhy, stageText, type Failure } from './words.ts'

export type TestState =
  | { phase: 'running'; account: M3Account }
  | { phase: 'done'; account: M3Account; result: M3TestResponse['result']; confirmed: boolean }
  | { phase: 'failed'; account: M3Account; failure: Failure }

export interface TestDialogProps {
  state: TestState | null
  /** test again with the warning confirmed */
  onConfirmAndTest: (account: M3Account) => void
  onClose: () => void
}

export function TestDialog({ state, onConfirmAndTest, onClose }: TestDialogProps) {
  const [ticked, setTicked] = useState(false)
  if (state === null) return null
  const close = () => {
    setTicked(false)
    onClose()
  }
  const confirmAndTest = (account: M3Account) => {
    setTicked(false)
    onConfirmAndTest(account)
  }
  const address = state.account.from_address
  const warned = state.phase === 'done' && state.result.from_gate === 'unaligned_warned'
  const needsConfirm = warned && state.phase === 'done' && !state.confirmed

  return (
    <Dialog
      open
      title={t('accounts.test.title', { address })}
      onClose={close}
      footer={
        <>
          {needsConfirm && (
            <Button variant="primary" disabled={!ticked} onClick={() => confirmAndTest(state.account)}>
              {t('accounts.test.again')}
            </Button>
          )}
          <Button onClick={close}>{t('accounts.test.done')}</Button>
        </>
      }
    >
      <div className="flex flex-col gap-3" aria-live="polite">
        {state.phase === 'running' && (
          <p className="flex items-center gap-2 text-base text-fg-muted">
            <Spinner label="" />
            {t('accounts.test.working')}
          </p>
        )}
        {state.phase === 'failed' && <ErrorNote failure={state.failure} />}
        {state.phase === 'done' && (
          <>
            <section className="flex flex-col gap-1">
              <h3 className="text-base font-semibold text-fg">{t('accounts.test.connection')}</h3>
              <p className="flex flex-wrap items-center gap-2 text-base text-fg">
                <StatusBadge status={state.result.ok ? 'valid' : 'invalid'} label={state.result.ok ? t('accounts.test.works') : t('accounts.test.fails')} />
                <span>{stageText(state.result.stage)}</span>
              </p>
            </section>
            <section className="flex flex-col gap-1">
              <h3 className="text-base font-semibold text-fg">{t('accounts.test.fromCheck')}</h3>
              <p className="flex flex-wrap items-center gap-2 text-base text-fg">
                <StatusBadge status={fromGateToken(state.result.from_gate)} label={fromGateLabel(state.result.from_gate)} />
                <span>{fromGateWhy(state.result.from_gate)}</span>
              </p>
              {warned && state.confirmed && <p className="text-sm text-fg-muted">{t('accounts.test.confirmed')}</p>}
            </section>
            {needsConfirm && (
              <div className="flex flex-col gap-2 rounded-md bg-status-risky-bg px-3 py-3 text-status-risky">
                <p className="text-base">{t('accounts.ack.body')}</p>
                <Checkbox name="acknowledge" label={t('accounts.ack.checkboxUse')} checked={ticked} onChange={(e) => setTicked(e.target.checked)} />
              </div>
            )}
          </>
        )}
      </div>
    </Dialog>
  )
}

// SMTP accounts: the user's own sender accounts at module 3 (the list, Add, Add many, Test, Remove).
//
// Every word comes from src/copy (accounts.ts, errors.ts); every state of an account is shown as a word AND a colour. What module 3 said about a failed login or about a
// From address never arrives here (the API sends words of its own), so nothing on this page can show it.
import { useCallback, useEffect, useRef, useState } from 'react'
import { balancesApi, m3Api } from '../../api/endpoints.ts'
import { isAbortError, isApiError } from '../../api/errors.ts'
import type { M3Account, M3Entitlement } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { Button, ConfirmDialog, EmptyState, PageHeader, Pagination, StatusBadge, Table, useCursorPager, useToast, type Column } from '../../ui/index.ts'
import { AccountOutcome } from './AccountOutcome.tsx'
import { AddAccountForm } from './AddAccountForm.tsx'
import { BulkAddPanel } from './BulkAddPanel.tsx'
import { ErrorNote } from './ErrorNote.tsx'
import { TestDialog, type TestState } from './TestDialog.tsx'
import { entitlementLine, failureOf, fromGateLabel, fromGateToken, fromGateWhy, statusLabel, statusReason, statusToken, verificationLine, type Failure } from './words.ts'

type Panel = 'none' | 'add' | 'bulk'

export function AccountsPage() {
  const toast = useToast()
  const pager = useCursorPager()
  const [accounts, setAccounts] = useState<M3Account[]>([])
  const [nextCursor, setNextCursor] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [loadFailure, setLoadFailure] = useState<Failure | null>(null)
  const [reloadKey, setReloadKey] = useState(0)

  const [entitlement, setEntitlement] = useState<M3Entitlement | null>(null)
  const [panel, setPanel] = useState<Panel>('none')
  const [added, setAdded] = useState<M3Account | null>(null)
  const [test, setTest] = useState<TestState | null>(null)
  const testRun = useRef(0)
  const [removing, setRemoving] = useState<M3Account | null>(null)
  const [removeBusy, setRemoveBusy] = useState(false)
  const [removeFailure, setRemoveFailure] = useState<Failure | null>(null)
  const addButton = useRef<HTMLButtonElement>(null)
  const bulkButton = useRef<HTMLButtonElement>(null)

  // the list: the first page, and the page the pager is on; read again when something changed (reloadKey)
  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    m3Api.list({ cursor: pager.cursor }, { signal: controller.signal }).then(
      (page) => {
        setAccounts(page.accounts)
        setNextCursor(page.next_cursor)
        setLoadFailure(null)
        setLoading(false)
      },
      (err: unknown) => {
        if (isAbortError(err) || controller.signal.aborted) return
        setLoadFailure(failureOf(err))
        setLoading(false)
      },
    )
    return () => controller.abort()
  }, [pager.cursor, reloadKey])

  // whether sending through the user's own accounts is switched on: said once, at the top. A failure says nothing (the list and the API are the authority).
  useEffect(() => {
    const controller = new AbortController()
    balancesApi.get({ signal: controller.signal }).then(
      (b) => setEntitlement(b.m3),
      () => {},
    )
    return () => controller.abort()
  }, [])

  // read the page again (a test may have changed an account), or start from the first page (an account was added or removed)
  const refresh = useCallback(() => setReloadKey((k) => k + 1), [])
  const resetPager = pager.reset
  const reload = useCallback(() => {
    resetPager()
    refresh()
  }, [resetPager, refresh])

  const openPanel = (next: Panel) => setPanel((p) => (p === next ? 'none' : next))
  const closePanel = (which: Exclude<Panel, 'none'>) => {
    setPanel('none')
    ;(which === 'add' ? addButton : bulkButton).current?.focus()
  }

  // ---- the Test button ----

  const runTest = async (account: M3Account, acknowledge: boolean) => {
    const run = ++testRun.current
    setTest({ phase: 'running', account })
    try {
      const res = await m3Api.test(account.id, acknowledge ? { acknowledge: true } : {})
      if (run !== testRun.current) return // the dialog was closed, or another test started: this answer belongs to nobody
      setTest({ phase: 'done', account, result: res.result, confirmed: acknowledge })
      refresh() // the login and the From address check of the account may have changed
    } catch (err) {
      if (isAbortError(err) || run !== testRun.current) return
      setTest({ phase: 'failed', account, failure: failureOf(err) })
      if (isApiError(err) && err.code === 'account_outcome_unknown') refresh()
    }
  }
  const closeTest = () => {
    testRun.current++
    setTest(null)
  }

  // ---- Remove ----

  const askRemove = (account: M3Account) => {
    setRemoveFailure(null)
    setRemoving(account)
  }
  const confirmRemove = async () => {
    if (removing === null) return
    setRemoveBusy(true)
    setRemoveFailure(null)
    try {
      await m3Api.remove(removing.id)
      toast.notify(t('accounts.remove.done', { address: removing.from_address }), { tone: 'success' })
      setRemoving(null)
      reload()
    } catch (err) {
      if (isAbortError(err)) return
      const failure = failureOf(err)
      setRemoving(null)
      setRemoveFailure(failure)
      // a removal whose answer was lost, or of an account that is already gone: the list says what is true now
      if (isApiError(err) && (err.code === 'account_outcome_unknown' || err.code === 'not_found')) reload()
    } finally {
      setRemoveBusy(false)
    }
  }

  const notice = entitlement === null ? null : entitlementLine(entitlement)
  const noticeIsProblem = entitlement !== null && (!entitlement.enabled || entitlement.state === 'suspended')

  const columns: Column<M3Account>[] = [
    {
      key: 'address',
      header: t('accounts.col.address'),
      rowHeader: true,
      cell: (a) => (
        <div className="flex flex-col">
          <span className="break-all">{a.from_address}</span>
          {a.from_name !== null && a.from_name !== '' && <span className="text-xs font-normal text-fg-muted">{t('accounts.row.name', { name: a.from_name })}</span>}
        </div>
      ),
    },
    {
      key: 'server',
      header: t('accounts.col.server'),
      cell: (a) => (
        <div className="flex flex-col text-sm">
          <span className="break-all">
            {a.smtp_host}:{a.smtp_port}
          </span>
          <span className="break-all text-xs text-fg-muted">{t('accounts.row.login', { username: a.smtp_username })}</span>
          <span className="text-xs text-fg-muted">{a.daily_limit === 1 ? t('accounts.row.limitOne') : t('accounts.row.limit', { count: a.daily_limit })}</span>
        </div>
      ),
    },
    {
      key: 'status',
      header: t('accounts.col.status'),
      cell: (a) => {
        const reason = statusReason(a)
        return (
          <div className="flex max-w-xs flex-col items-start gap-1 text-sm">
            <StatusBadge status={statusToken(a.status)} label={statusLabel(a.status)} />
            {reason !== null && <span className="text-xs text-fg-muted">{reason}</span>}
            <span className="text-xs text-fg-muted">{verificationLine(a)}</span>
          </div>
        )
      },
    },
    {
      key: 'fromCheck',
      header: t('accounts.col.fromCheck'),
      cell: (a) => (
        <div className="flex max-w-xs flex-col items-start gap-1 text-sm">
          <StatusBadge status={fromGateToken(a.from_gate)} label={fromGateLabel(a.from_gate)} />
          <span className="text-xs text-fg-muted">{fromGateWhy(a.from_gate)}</span>
        </div>
      ),
    },
    {
      key: 'actions',
      header: t('accounts.col.actions'),
      cell: (a) => (
        <div className="flex gap-2">
          <Button size="sm" aria-label={t('accounts.test.buttonFor', { address: a.from_address })} busy={test?.phase === 'running' && test.account.id === a.id} onClick={() => void runTest(a, false)}>
            {t('accounts.test.button')}
          </Button>
          <Button size="sm" aria-label={t('accounts.remove.buttonFor', { address: a.from_address })} onClick={() => askRemove(a)}>
            {t('accounts.remove.button')}
          </Button>
        </div>
      ),
    },
  ]

  return (
    <section className="flex flex-col gap-4">
      <PageHeader
        title={t('accounts.title')}
        description={t('accounts.intro')}
        actions={
          <>
            <Button ref={addButton} variant="primary" aria-expanded={panel === 'add'} onClick={() => openPanel('add')}>
              {t('accounts.add.open')}
            </Button>
            <Button ref={bulkButton} aria-expanded={panel === 'bulk'} onClick={() => openPanel('bulk')}>
              {t('accounts.bulk.open')}
            </Button>
          </>
        }
      />

      {notice !== null && (
        <p className={noticeIsProblem ? 'rounded-md bg-status-risky-bg px-3 py-2 text-base text-status-risky' : 'text-base text-fg-muted'}>{notice}</p>
      )}

      {added !== null && <AccountOutcome account={added} onDismiss={() => setAdded(null)} />}

      {panel === 'add' && (
        <AddAccountForm
          onAdded={(account) => {
            setPanel('none')
            setAdded(account)
            reload()
          }}
          onCancel={() => closePanel('add')}
          onOutcomeUnknown={reload}
        />
      )}
      {panel === 'bulk' && <BulkAddPanel onChanged={reload} onCancel={() => closePanel('bulk')} />}

      {removeFailure !== null && <ErrorNote failure={removeFailure} />}
      {loadFailure !== null && (
        <div className="flex flex-col items-start gap-2">
          <p className="text-base text-fg">{t('accounts.loadError')}</p>
          <ErrorNote failure={loadFailure} />
          <Button onClick={reload}>{t('common.retry')}</Button>
        </div>
      )}

      {loadFailure === null && (
        <>
          <Table
            caption={t('accounts.list.caption')}
            columns={columns}
            rows={accounts}
            rowKey={(a) => a.id}
            rowTestId={(a) => `account-row-${a.id}`}
            loading={loading}
            empty={
              <EmptyState title={t('accounts.empty.title')} description={t('accounts.empty.body')} />
            }
          />
          {(pager.hasPrevious || nextCursor !== null) && (
            <Pagination
              page={pager.page}
              hasPrevious={pager.hasPrevious}
              hasNext={nextCursor !== null}
              busy={loading}
              onPrevious={pager.goPrevious}
              onNext={() => pager.goNext(nextCursor)}
            />
          )}
        </>
      )}

      <TestDialog state={test} onConfirmAndTest={(account) => void runTest(account, true)} onClose={closeTest} />

      <ConfirmDialog
        open={removing !== null}
        destructive
        busy={removeBusy}
        title={t('accounts.remove.title', { address: removing?.from_address ?? '' })}
        description={t('accounts.remove.body')}
        confirmLabel={t('accounts.remove.confirm')}
        onConfirm={() => void confirmRemove()}
        onCancel={() => setRemoving(null)}
      />
    </section>
  )
}

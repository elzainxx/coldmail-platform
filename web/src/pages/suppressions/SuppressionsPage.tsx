// The suppression list: the addresses every future job of the user leaves out. Search (debounced), add one, import a file, remove one after a confirmation that
// says what it means (more strongly for an address that unsubscribed itself).
import { useCallback, useEffect, useState, type FormEvent } from 'react'
import { suppressionsApi } from '../../api/endpoints.ts'
import { isAbortError, isApiError } from '../../api/errors.ts'
import type { Suppression } from '../../api/types.ts'
import { maybeT, t } from '../../copy/index.ts'
import { addressReasonOf, malformedReasonText } from '../../copy/words.ts'
import { Button, Card, ConfirmDialog, EmptyState, FormField, PageHeader, Pagination, Table, TextInput, useCursorPager, useToast, type Column } from '../../ui/index.ts'
import { FailureNote, failureOf, type Failure } from './Failure.tsx'
import { ImportPanel } from './ImportPanel.tsx'

/** How long the search waits for the user to stop typing before it asks the service. */
export const SEARCH_DEBOUNCE_MS = 300
/** The API refuses a search longer than this many bytes. */
const MAX_SEARCH = 254

const WHEN = new Intl.DateTimeFormat('en-US', { dateStyle: 'medium', timeStyle: 'short' })

function formatWhen(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : WHEN.format(d)
}

/** How an address got on the list, in words (a source the API grows later reads "Added", never the word). */
export function sourceText(source: string): string {
  return maybeT(`suppressions.source.${source}`) ?? t('suppressions.source.other')
}

/** A row as the table has it: the entry and its place on the page (the n of `suppression-row-<n>`, counted from 1). */
interface Row extends Suppression {
  n: number
}

export function SuppressionsPage() {
  const toast = useToast()
  const pager = useCursorPager()
  const [rows, setRows] = useState<Row[]>([])
  const [nextCursor, setNextCursor] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [loadFailure, setLoadFailure] = useState<Failure | null>(null)
  const [reloadKey, setReloadKey] = useState(0)

  // what is in the search box, and what the list was last asked for (it follows the box after the user has stopped typing)
  const [search, setSearch] = useState('')
  const [query, setQuery] = useState('')
  const { reset: resetPager } = pager
  useEffect(() => {
    const timer = setTimeout(() => {
      // one batch: the new search and the first page together, so the list is never asked for page 3 of a search the user has just changed
      setQuery(search.trim())
      resetPager()
    }, SEARCH_DEBOUNCE_MS)
    return () => clearTimeout(timer)
  }, [search, resetPager])

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    suppressionsApi.list({ q: query, cursor: pager.cursor }, { signal: controller.signal }).then(
      (page) => {
        setRows(page.entries.map((e, i) => ({ ...e, n: i + 1 })))
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
  }, [query, pager.cursor, reloadKey])

  const reload = useCallback(() => {
    resetPager()
    setReloadKey((k) => k + 1)
  }, [resetPager])

  // ---- add ----

  const [email, setEmail] = useState('')
  const [adding, setAdding] = useState(false)
  const [addError, setAddError] = useState<string | null>(null)
  const [addFailure, setAddFailure] = useState<Failure | null>(null)

  const add = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    if (adding) return
    const form = e.currentTarget
    setAddFailure(null)
    const address = email.trim()
    if (address === '') {
      setAddError(t('suppressions.add.empty'))
      return
    }
    setAddError(null)
    setAdding(true)
    try {
      const res = await suppressionsApi.add(address)
      // the answer shows the address in the platform's own form (trimmed, lower case): that is the one the list holds
      toast.notify(t(res.already === true ? 'suppressions.add.already' : 'suppressions.add.done', { email: res.entry.email }), { tone: res.already === true ? 'info' : 'success' })
      setEmail('')
      reload()
      ;(form.elements.namedItem('email') as HTMLInputElement | null)?.focus() // ready for the next one
    } catch (err) {
      if (isAbortError(err)) return
      const reason = isApiError(err) && err.code === 'validation_failed' ? addressReasonOf(err) : null
      if (reason !== null) setAddError(malformedReasonText(reason))
      else setAddFailure(failureOf(err))
    } finally {
      setAdding(false)
    }
  }

  // ---- remove ----

  const [removing, setRemoving] = useState<Suppression | null>(null)
  const [removeBusy, setRemoveBusy] = useState(false)
  const [removeFailure, setRemoveFailure] = useState<Failure | null>(null)

  const confirmRemove = async () => {
    if (removing === null) return
    setRemoveBusy(true)
    setRemoveFailure(null)
    try {
      const res = await suppressionsApi.remove(removing.email)
      toast.notify(t(res.removed ? 'suppressions.remove.done' : 'suppressions.remove.gone', { email: removing.email }), { tone: res.removed ? 'success' : 'info' })
      setRemoving(null)
      reload()
    } catch (err) {
      if (isAbortError(err)) return
      setRemoving(null)
      setRemoveFailure(failureOf(err))
    } finally {
      setRemoveBusy(false)
    }
  }
  const byUnsubscribe = removing?.source === 'unsubscribe'

  const columns: Column<Row>[] = [
    { key: 'address', header: t('suppressions.col.address'), rowHeader: true, cell: (r) => <span className="break-all">{r.email}</span> },
    { key: 'source', header: t('suppressions.col.source'), cell: (r) => sourceText(r.source) },
    {
      key: 'when',
      header: t('suppressions.col.when'),
      cell: (r) => (
        <time dateTime={r.created_at} className="whitespace-nowrap">
          {formatWhen(r.created_at)}
        </time>
      ),
    },
    {
      key: 'actions',
      header: t('suppressions.col.actions'),
      cell: (r) => (
        <Button size="sm" aria-label={t('suppressions.remove.buttonFor', { email: r.email })} onClick={() => { setRemoveFailure(null); setRemoving(r) }}>
          {t('suppressions.remove.button')}
        </Button>
      ),
    },
  ]

  return (
    <section className="flex flex-col gap-4">
      <PageHeader title={t('suppressions.title')} description={t('suppressions.intro')} />

      <div className="grid gap-4 lg:grid-cols-2">
        <Card title={t('suppressions.add.title')}>
          <form method="post" onSubmit={(e) => void add(e)} noValidate className="flex flex-col gap-3">
            {addFailure !== null && <FailureNote failure={addFailure} />}
            <FormField label={t('suppressions.add.label')} hint={t('suppressions.add.hint')} error={addError}>
              <TextInput
                name="email"
                type="email"
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                value={email}
                onChange={(e) => setEmail(e.target.value)}
              />
            </FormField>
            <div>
              <Button type="submit" variant="primary" busy={adding}>
                {t('suppressions.add.submit')}
              </Button>
            </div>
          </form>
        </Card>
        <ImportPanel onImported={reload} />
      </div>

      <FormField label={t('suppressions.search.label')} hint={t('suppressions.search.hint')}>
        <TextInput type="search" name="q" autoComplete="off" spellCheck={false} maxLength={MAX_SEARCH} value={search} onChange={(e) => setSearch(e.target.value)} />
      </FormField>

      {removeFailure !== null && <FailureNote failure={removeFailure} />}
      {loadFailure !== null ? (
        <div className="flex flex-col items-start gap-2">
          <p className="text-base text-fg">{t('suppressions.loadError')}</p>
          <FailureNote failure={loadFailure} />
          <Button onClick={reload}>{t('common.retry')}</Button>
        </div>
      ) : (
        <>
          <Table
            caption={t('suppressions.table.caption')}
            columns={columns}
            rows={rows}
            rowKey={(r) => r.email}
            rowTestId={(r) => `suppression-row-${r.n}`}
            loading={loading}
            empty={
              query === '' ? (
                <EmptyState title={t('suppressions.empty.title')} description={t('suppressions.empty.body')} />
              ) : (
                <EmptyState title={t('suppressions.empty.search', { query })} />
              )
            }
          />
          {(pager.hasPrevious || nextCursor !== null) && (
            <Pagination page={pager.page} hasPrevious={pager.hasPrevious} hasNext={nextCursor !== null} busy={loading} onPrevious={pager.goPrevious} onNext={() => pager.goNext(nextCursor)} />
          )}
        </>
      )}

      <ConfirmDialog
        open={removing !== null}
        destructive
        busy={removeBusy}
        title={t('suppressions.remove.title', { email: removing?.email ?? '' })}
        description={byUnsubscribe ? t('suppressions.remove.bodyUnsubscribed') : t('suppressions.remove.body')}
        confirmLabel={byUnsubscribe ? t('suppressions.remove.confirmUnsubscribed') : t('suppressions.remove.confirm')}
        onConfirm={() => void confirmRemove()}
        onCancel={() => setRemoving(null)}
      />
    </section>
  )
}

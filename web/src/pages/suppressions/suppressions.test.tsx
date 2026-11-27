// The suppression list screen: the table and its numbered rows, the debounced search, the cursor paging, add (and every way an address is refused, in words), remove
// with a confirmation that names the consequence (more strongly for an address that unsubscribed itself), and the CSV import. Every property has a paired control.
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Suppression } from '../../api/types.ts'
import { t } from '../../copy/index.ts'
import { jsonResponse, mockApi, renderApp, resetForTest, signInForTest } from '../../test/helpers.tsx'
import { SEARCH_DEBOUNCE_MS } from './SuppressionsPage.tsx'

const LEAK = 'LEAK-the-server-sentence'

const entry = (email: string, source: string = 'manual', created_at = '2026-11-20T10:00:00Z'): Suppression => ({ email, source: source as Suppression['source'], created_at })

type Fetch = ReturnType<typeof mockApi>

function requests(fn: Fetch, method: string, path: string) {
  return fn.mock.calls
    .map(([input, init]) => ({ url: new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, window.location.origin), init: init ?? {} }))
    .filter((c) => (c.init.method ?? 'GET').toUpperCase() === method && c.url.pathname === path)
}
const lists = (fn: Fetch) => requests(fn, 'GET', '/v1/suppressions')
const bodyOf = (c: { init: RequestInit }): Record<string, unknown> => JSON.parse(String(c.init.body)) as Record<string, unknown>

function apiError(code: string, status: number, details?: Record<string, unknown>): Response {
  return jsonResponse({ error: { code, message: LEAK, request_id: 'req_00000000000000000000000000000001', ...(details === undefined ? {} : { details }) } }, status)
}

async function openList(routes: Record<string, unknown> = {}): Promise<Fetch> {
  await signInForTest()
  const fn = mockApi({ 'GET /v1/suppressions': { entries: [], next_cursor: null }, 'GET /v1/me': { email: 'person@example.test' }, ...routes })
  renderApp('/suppressions')
  await screen.findByRole('heading', { level: 1, name: t('suppressions.title') })
  return fn
}

const main = () => screen.getByRole('main')
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } })
const searchBox = () => screen.getByRole('searchbox', { name: new RegExp(`^${t('suppressions.search.label')}`) })
const addBox = () => screen.getByLabelText(new RegExp(`^${t('suppressions.add.label')}`))
const clickAdd = () => fireEvent.click(screen.getByRole('button', { name: t('suppressions.add.submit') }))
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

function expectNoInternalText() {
  const text = document.body.textContent ?? ''
  expect(text).not.toMatch(/\b[a-z]+_[a-z0-9_]+\b/)
  expect(text).not.toContain(LEAK)
}

const blobText = (blob: Blob): Promise<string> =>
  new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result))
    reader.onerror = () => reject(reader.error)
    reader.readAsText(blob)
  })

beforeEach(() => {
  resetForTest()
})
afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  resetForTest()
})

// ---------------------------------------------------------------------------------------------------------------------------------------------------

describe('the table', () => {
  it('numbers the rows from 1, and shows each address with how it got there, in words, and when', async () => {
    await openList({
      'GET /v1/suppressions': {
        entries: [entry('ann@example.test', 'unsubscribe', '2026-11-21T09:30:00Z'), entry('bob@example.test', 'manual'), entry('cy@example.test', 'import'), entry('di@example.test', 'zapier')],
        next_cursor: null,
      },
    })
    const first = await screen.findByTestId('suppression-row-1')
    expect(within(first).getByText('ann@example.test')).toBeInTheDocument()
    expect(within(first).getByText(t('suppressions.source.unsubscribe'))).toBeInTheDocument()
    expect(first.querySelector('time')?.getAttribute('datetime')).toBe('2026-11-21T09:30:00Z')
    expect(first.querySelector('time')?.textContent).not.toBe('')
    expect(within(screen.getByTestId('suppression-row-2')).getByText(t('suppressions.source.manual'))).toBeInTheDocument()
    expect(within(screen.getByTestId('suppression-row-3')).getByText(t('suppressions.source.import'))).toBeInTheDocument()
    // a way of getting there the app has no words for reads "Added", never the word
    expect(within(screen.getByTestId('suppression-row-4')).getByText(t('suppressions.source.other'))).toBeInTheDocument()
    expect(document.body.textContent).not.toContain('zapier')
    expect(screen.queryByTestId('suppression-row-5')).not.toBeInTheDocument() // control: as many rows as entries
    expect(screen.queryByText(t('suppressions.empty.title'))).not.toBeInTheDocument()
    expectNoInternalText()
  })

  it('explains what the list is for when it is empty, and says no address matches when a search finds none', async () => {
    const fn = await openList()
    expect(await screen.findByText(t('suppressions.empty.title'))).toBeInTheDocument()
    expect(screen.getByText(t('suppressions.empty.body'))).toBeInTheDocument()
    type(searchBox(), 'zed')
    expect(await screen.findByText(t('suppressions.empty.search', { query: 'zed' }))).toBeInTheDocument()
    expect(screen.queryByText(t('suppressions.empty.title'))).not.toBeInTheDocument() // control: the two empty states are not the same one
    expect(lists(fn).at(-1)?.url.searchParams.get('q')).toBe('zed')
  })

  it('says it could not load the list, in words, and tries again on request', async () => {
    let healthy = false
    await openList({ 'GET /v1/suppressions': () => (healthy ? { entries: [entry('ann@example.test')], next_cursor: null } : apiError('internal_error', 500)) })
    expect(await screen.findByText(t('suppressions.loadError'))).toBeInTheDocument()
    expect(screen.getByText(t('error.internal_error'))).toBeInTheDocument()
    expect(screen.getByText('Reference req_00000000000000000000000000000001')).toBeInTheDocument()
    expectNoInternalText()
    healthy = true
    fireEvent.click(screen.getByRole('button', { name: t('common.retry') }))
    await screen.findByTestId('suppression-row-1')
    expect(screen.queryByText(t('suppressions.loadError'))).not.toBeInTheDocument()
  })
})

describe('the search', () => {
  it('waits for the user to stop typing: one request for "ann", none for "a" or "an"', async () => {
    const fn = await openList({ 'GET /v1/suppressions': { entries: [entry('ann@example.test')], next_cursor: null } })
    await screen.findByTestId('suppression-row-1')
    expect(lists(fn)).toHaveLength(1)
    // keystrokes closer together than the wait, with real time between them: a search that asked at once would have asked for "a" and "an" by now
    type(searchBox(), 'a')
    await sleep(SEARCH_DEBOUNCE_MS / 3)
    type(searchBox(), 'an')
    await sleep(SEARCH_DEBOUNCE_MS / 3)
    type(searchBox(), 'ann')
    expect(lists(fn)).toHaveLength(1) // nothing yet: the user may still be typing
    await waitFor(() => expect(lists(fn)).toHaveLength(2))
    await sleep(SEARCH_DEBOUNCE_MS + 100) // and nothing more comes
    expect(lists(fn).map((c) => c.url.searchParams.get('q'))).toEqual([null, 'ann'])
  })

  it('control: a search that is not changed asks for nothing, however long it waits; one that is changed again asks again', async () => {
    const fn = await openList()
    await screen.findByText(t('suppressions.empty.title'))
    await sleep(SEARCH_DEBOUNCE_MS + 100)
    expect(lists(fn)).toHaveLength(1)
    type(searchBox(), 'x')
    await waitFor(() => expect(lists(fn)).toHaveLength(2))
    type(searchBox(), 'xy')
    await waitFor(() => expect(lists(fn)).toHaveLength(3))
    expect(lists(fn).map((c) => c.url.searchParams.get('q'))).toEqual([null, 'x', 'xy'])
  })

  it('sends the search without the spaces around it, and clearing the box asks for the whole list again', async () => {
    const fn = await openList()
    await screen.findByText(t('suppressions.empty.title'))
    type(searchBox(), '  Ann  ')
    await waitFor(() => expect(lists(fn)).toHaveLength(2))
    expect(lists(fn)[1]?.url.searchParams.get('q')).toBe('Ann')
    type(searchBox(), '')
    await waitFor(() => expect(lists(fn)).toHaveLength(3))
    expect(lists(fn)[2]?.url.searchParams.has('q')).toBe(false)
  })

  it('starts again from the first page when the search changes', async () => {
    const fn = await openList({
      'GET /v1/suppressions': (url: URL) => (url.searchParams.get('cursor') === 'c1' ? { entries: [entry('page2@example.test')], next_cursor: null } : { entries: [entry('page1@example.test')], next_cursor: 'c1' }),
    })
    await screen.findByText('page1@example.test')
    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.next') }))
    await screen.findByText('page2@example.test')
    type(searchBox(), 'page')
    await waitFor(() => expect(lists(fn).at(-1)?.url.searchParams.get('q')).toBe('page'))
    expect(lists(fn).at(-1)?.url.searchParams.has('cursor')).toBe(false)
    await screen.findByText('page1@example.test')
  })
})

describe('the paging', () => {
  it('asks for the next page with the cursor it was given, and the first page without one', async () => {
    const fn = await openList({
      'GET /v1/suppressions': (url: URL) => (url.searchParams.get('cursor') === 'c1' ? { entries: [entry('page2@example.test')], next_cursor: null } : { entries: [entry('page1@example.test')], next_cursor: 'c1' }),
    })
    await screen.findByText('page1@example.test')
    expect(lists(fn)[0]?.url.searchParams.has('cursor')).toBe(false)
    expect(screen.getByRole('button', { name: t('common.pagination.previous') })).toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.next') }))
    await screen.findByText('page2@example.test')
    expect(lists(fn).at(-1)?.url.searchParams.get('cursor')).toBe('c1')
    expect(screen.getByTestId('suppression-row-1')).toHaveTextContent('page2@example.test') // numbered from 1 on every page
    expect(screen.getByRole('button', { name: t('common.pagination.next') })).toBeDisabled() // the last page has no next
    fireEvent.click(screen.getByRole('button', { name: t('common.pagination.previous') }))
    await screen.findByText('page1@example.test')
  })

  it('control: a list that fits on one page has no paging', async () => {
    await openList({ 'GET /v1/suppressions': { entries: [entry('ann@example.test')], next_cursor: null } })
    await screen.findByTestId('suppression-row-1')
    expect(screen.queryByRole('navigation', { name: t('common.pagination.label') })).not.toBeInTheDocument()
  })
})

// ---------------------------------------------------------------------------------------------------------------------------------------------------

describe('Add', () => {
  it('adds the address, says so, empties the box, and reads the list again', async () => {
    let list: Suppression[] = []
    const fn = await openList({
      'GET /v1/suppressions': () => ({ entries: list, next_cursor: null }),
      'POST /v1/suppressions': () => {
        list = [entry('ann@example.test')]
        return [201, { entry: entry('ann@example.test') }]
      },
    })
    await screen.findByText(t('suppressions.empty.title'))
    type(addBox(), '  Ann@Example.test ')
    clickAdd()
    // the toast shows the address as the platform holds it (trimmed, lower case), not as it was typed
    expect(await screen.findByText(t('suppressions.add.done', { email: 'ann@example.test' }))).toBeInTheDocument()
    expect(requests(fn, 'POST', '/v1/suppressions')).toHaveLength(1)
    expect(bodyOf(requests(fn, 'POST', '/v1/suppressions')[0]!)).toEqual({ email: 'Ann@Example.test' })
    expect(addBox()).toHaveValue('')
    await screen.findByTestId('suppression-row-1')
    expect(lists(fn)).toHaveLength(2)
  })

  it('says an address that was already there as such (a different line from a new one)', async () => {
    await openList({ 'POST /v1/suppressions': [200, { entry: entry('ann@example.test', 'unsubscribe'), already: true }] })
    await screen.findByText(t('suppressions.empty.title'))
    type(addBox(), 'ann@example.test')
    clickAdd()
    expect(await screen.findByText(t('suppressions.add.already', { email: 'ann@example.test' }))).toBeInTheDocument()
    expect(screen.queryByText(t('suppressions.add.done', { email: 'ann@example.test' }))).not.toBeInTheDocument()
  })

  it('does not send an empty box, and says what is missing', async () => {
    const fn = await openList()
    await screen.findByText(t('suppressions.empty.title'))
    clickAdd()
    expect(await screen.findByRole('alert')).toHaveTextContent(t('suppressions.add.empty'))
    expect(addBox()).toHaveAttribute('aria-invalid', 'true')
    expect(requests(fn, 'POST', '/v1/suppressions')).toHaveLength(0)
    type(addBox(), 'ann@example.test') // control: with an address in it the same button sends
    clickAdd()
    await waitFor(() => expect(requests(fn, 'POST', '/v1/suppressions').length).toBe(1))
  })

  it.each([
    ['empty', 'common.malformed.empty'],
    ['no_at', 'common.malformed.no_at'],
    ['multiple_at', 'common.malformed.multiple_at'],
    ['empty_local', 'common.malformed.empty_local'],
    ['empty_domain', 'common.malformed.empty_domain'],
    ['bad_idna', 'common.malformed.bad_idna'],
    ['part_empty', 'common.malformed.part_empty'],
    ['local_too_long', 'common.malformed.local_too_long'],
    ['domain_too_long', 'common.malformed.domain_too_long'],
    ['domain_no_dot', 'common.malformed.domain_no_dot'],
    ['cannot_store', 'common.malformed.cannot_store'],
    ['a_reason_from_the_future', 'common.malformed.other'],
  ] as const)('says why an address was refused (%s) in words, on the box, without the word and without the address', async (reason, key) => {
    await openList({ 'POST /v1/suppressions': () => apiError('validation_failed', 400, { reason: 'the address is not valid', address_reason: reason }) })
    await screen.findByText(t('suppressions.empty.title'))
    type(addBox(), 'private-person@nowhere')
    clickAdd()
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t(key))
    expect(addBox()).toHaveAttribute('aria-invalid', 'true')
    expect(addBox()).toHaveAccessibleDescription(new RegExp(t(key).replace(/[.*+?^${}()|[\]\\]/g, '\\$&')))
    expect(alert.textContent).not.toContain(reason === 'a_reason_from_the_future' ? reason : `_${reason}`)
    expectNoInternalText()
    expect(screen.queryByText(t('suppressions.add.done', { email: 'x' }))).not.toBeInTheDocument()
  })

  it('shows any other failure as its line with the reference, and keeps the address in the box', async () => {
    await openList({ 'POST /v1/suppressions': () => apiError('internal_error', 500) })
    await screen.findByText(t('suppressions.empty.title'))
    type(addBox(), 'ann@example.test')
    clickAdd()
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('error.internal_error'))
    expect(alert).toHaveTextContent('Reference req_00000000000000000000000000000001')
    expect(addBox()).toHaveValue('ann@example.test')
    expectNoInternalText()
  })
})

// ---------------------------------------------------------------------------------------------------------------------------------------------------

describe('Remove', () => {
  const rowWith = (source: string) => ({ 'GET /v1/suppressions': { entries: [entry('ann@example.test', source)], next_cursor: null } })
  const clickRemove = async () => {
    const row = await screen.findByTestId('suppression-row-1')
    const button = within(row).getByRole('button', { name: t('suppressions.remove.buttonFor', { email: 'ann@example.test' }) })
    button.focus()
    fireEvent.click(button)
    return button
  }

  it.each([
    ['manual', 'suppressions.remove.body', 'suppressions.remove.confirm'],
    ['import', 'suppressions.remove.body', 'suppressions.remove.confirm'],
    ['unsubscribe', 'suppressions.remove.bodyUnsubscribed', 'suppressions.remove.confirmUnsubscribed'],
  ] as const)('an address that came by "%s" is confirmed with its own words', async (source, bodyKey, confirmKey) => {
    const fn = await openList({ ...rowWith(source), 'POST /v1/suppressions/remove': { removed: true } })
    await clickRemove()
    const dialog = await screen.findByRole('alertdialog', { name: t('suppressions.remove.title', { email: 'ann@example.test' }) })
    expect(within(dialog).getByText(t(bodyKey))).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: t(confirmKey) })).toBeInTheDocument()
    expect(requests(fn, 'POST', '/v1/suppressions/remove')).toHaveLength(0) // asking is not removing
    // control: the stronger words are for the address that unsubscribed itself, and only for it
    const strong = within(dialog).queryByText(t('suppressions.remove.bodyUnsubscribed')) !== null
    expect(strong).toBe(source === 'unsubscribe')
    expect(dialog.textContent?.includes('responsible')).toBe(source === 'unsubscribe')
    expect(dialog.textContent).toContain('mail') // every one says what removing means: the address can be mailed again
  })

  it('says plainly that the address can be mailed again, and for an unsubscribed one that the user is responsible', () => {
    expect(t('suppressions.remove.body')).toMatch(/able to mail this address again/)
    expect(t('suppressions.remove.bodyUnsubscribed')).toMatch(/unsubscribed from your messages themselves/)
    expect(t('suppressions.remove.bodyUnsubscribed')).toMatch(/you are responsible/)
    expect(t('suppressions.remove.body')).not.toMatch(/responsible/)
  })

  it('removes the address after the confirmation: one request with the address, a line that says so, and the list read again', async () => {
    const fn = await openList({ ...rowWith('manual'), 'POST /v1/suppressions/remove': { removed: true } })
    await clickRemove()
    fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: t('suppressions.remove.confirm') }))
    expect(await screen.findByText(t('suppressions.remove.done', { email: 'ann@example.test' }))).toBeInTheDocument()
    const sent = requests(fn, 'POST', '/v1/suppressions/remove')
    expect(sent).toHaveLength(1)
    expect(bodyOf(sent[0]!)).toEqual({ email: 'ann@example.test' })
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument())
    expect(lists(fn).length).toBeGreaterThanOrEqual(2)
  })

  it('says it differently when the address was no longer on the list', async () => {
    await openList({ ...rowWith('manual'), 'POST /v1/suppressions/remove': { removed: false } })
    await clickRemove()
    fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: t('suppressions.remove.confirm') }))
    expect(await screen.findByText(t('suppressions.remove.gone', { email: 'ann@example.test' }))).toBeInTheDocument()
    expect(screen.queryByText(t('suppressions.remove.done', { email: 'ann@example.test' }))).not.toBeInTheDocument()
  })

  it('control: Cancel and Escape remove nothing, and focus goes back to the Remove button', async () => {
    const fn = await openList({ ...rowWith('unsubscribe'), 'POST /v1/suppressions/remove': { removed: true } })
    let button = await clickRemove()
    const cancel = within(await screen.findByRole('alertdialog')).getByRole('button', { name: t('common.cancel') })
    expect(cancel).toHaveFocus() // the safe button has focus: Enter does not remove by accident
    fireEvent.click(cancel)
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument())
    expect(button).toHaveFocus()
    button = await clickRemove()
    fireEvent.keyDown(await screen.findByRole('alertdialog'), { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument())
    expect(button).toHaveFocus()
    expect(requests(fn, 'POST', '/v1/suppressions/remove')).toHaveLength(0)
  })

  it('shows a failure as its line, once the dialog is closed', async () => {
    await openList({ ...rowWith('manual'), 'POST /v1/suppressions/remove': () => apiError('internal_error', 500) })
    await clickRemove()
    fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: t('suppressions.remove.confirm') }))
    expect(await screen.findByRole('alert')).toHaveTextContent(t('error.internal_error'))
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expectNoInternalText()
  })
})

// ---------------------------------------------------------------------------------------------------------------------------------------------------

describe('Import', () => {
  const importPath = '/v1/suppressions/import'
  const choose = (name: string, text: string) => {
    const file = new File([text], name, { type: 'text/csv' })
    fireEvent.change(screen.getByLabelText(new RegExp(`^${t('suppressions.import.label')}`)), { target: { files: [file] } })
    return file
  }
  const clickImport = () => fireEvent.click(screen.getByRole('button', { name: t('suppressions.import.submit') }))

  it('sends the file as a CSV, and shows how many were added, were there already and were not valid', async () => {
    const fn = await openList({ [`POST ${importPath}`]: { added: 120, already_there: 3, invalid: 0, invalid_sample: [] } })
    await screen.findByText(t('suppressions.empty.title'))
    const file = choose('list.csv', 'email\nsecret-person@example.test\n')
    clickImport()
    expect(await screen.findByText(t('suppressions.import.summary', { added: 120, already: 3, invalid: 0 }))).toBeInTheDocument()
    const sent = requests(fn, 'POST', importPath)
    expect(sent).toHaveLength(1)
    expect((sent[0]!.init.headers as Record<string, string>)['Content-Type']).toBe('text/csv')
    expect(await blobText(sent[0]!.init.body as Blob)).toBe('email\nsecret-person@example.test\n')
    expect(sent[0]!.init.body).toBe(file) // the file itself, not a copy read into the page
    // control: with nothing invalid there is no table of lines and no note about them
    expect(screen.queryByRole('region', { name: t('suppressions.import.sampleCaption') })).not.toBeInTheDocument()
    expect(screen.queryByText(t('suppressions.import.sampleAll'))).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain('secret-person') // no address is shown back
    expect(lists(fn).length).toBeGreaterThanOrEqual(2) // the list is read again
  })

  it('shows the lines that were not valid with the line number and a word each, and says when the sample is not all of them', async () => {
    await openList({
      [`POST ${importPath}`]: {
        added: 5,
        already_there: 0,
        invalid: 25,
        invalid_sample: [
          { line: 3, reason: 'no_at' },
          { line: 7, reason: 'bad_line' },
          { line: 9, reason: 'domain_no_dot' },
          { line: 12, reason: 'a_reason_from_the_future' },
        ],
      },
    })
    await screen.findByText(t('suppressions.empty.title'))
    choose('list.csv', 'email\n')
    clickImport()
    const table = await screen.findByRole('region', { name: t('suppressions.import.sampleCaption') })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows.map((r) => [within(r).getByRole('rowheader').textContent, within(r).getAllByRole('cell')[0]?.textContent])).toEqual([
      ['3', t('common.malformed.no_at')],
      ['7', t('common.malformed.bad_line')],
      ['9', t('common.malformed.domain_no_dot')],
      ['12', t('common.malformed.other')],
    ])
    expect(screen.getByText(t('suppressions.import.sampleNote', { shown: 4, invalid: 25 }))).toBeInTheDocument()
    expect(screen.queryByText(t('suppressions.import.sampleAll'))).not.toBeInTheDocument()
    expectNoInternalText()
  })

  it('says every line is shown when the sample holds them all', async () => {
    await openList({ [`POST ${importPath}`]: { added: 1, already_there: 0, invalid: 1, invalid_sample: [{ line: 4, reason: 'no_at' }] } })
    await screen.findByText(t('suppressions.empty.title'))
    choose('list.csv', 'email\n')
    clickImport()
    expect(await screen.findByText(t('suppressions.import.sampleAll'))).toBeInTheDocument()
    expect(screen.queryByText(t('suppressions.import.sampleNote', { shown: 1, invalid: 1 }))).not.toBeInTheDocument()
  })

  it('does not send when no file is chosen', async () => {
    const fn = await openList({ [`POST ${importPath}`]: { added: 0, already_there: 0, invalid: 0, invalid_sample: [] } })
    await screen.findByText(t('suppressions.empty.title'))
    clickImport()
    expect(await screen.findByRole('alert')).toHaveTextContent(t('suppressions.import.nothing'))
    expect(requests(fn, 'POST', importPath)).toHaveLength(0)
    choose('list.csv', 'email\na@example.test\n') // control: with a file the same button sends
    clickImport()
    await waitFor(() => expect(requests(fn, 'POST', importPath)).toHaveLength(1))
  })

  it('says a file it cannot use in words (no email column), and any other refusal as its line', async () => {
    let answer: () => Response = () => apiError('validation_failed', 400, { reason: 'the header has no email column' })
    await openList({ [`POST ${importPath}`]: () => answer() })
    await screen.findByText(t('suppressions.empty.title'))
    choose('list.csv', 'name\nAnn\n')
    clickImport()
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(t('suppressions.import.badFile'))
    expect(alert.textContent).not.toContain('header has no') // the developer's sentence stays out
    expectNoInternalText()
    answer = () => apiError('too_many_rows', 413, { limit: 100000 })
    clickImport()
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(t('error.too_many_rows', { limit: '100,000' })))
    expect(screen.queryByText(t('suppressions.import.badFile'))).not.toBeInTheDocument()
    expectNoInternalText()
  })

  it('has no result before a file was imported', async () => {
    await openList()
    await screen.findByText(t('suppressions.empty.title'))
    expect(main().textContent).not.toMatch(/Added: \d/)
  })
})

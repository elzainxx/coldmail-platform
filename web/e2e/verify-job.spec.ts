// A verify job from a CSV list, through the screens (chunk A10 "verify from CSV and download"), with user a:
//
//   1  the whole story with the fixture list (web/e2e/fixtures/verify-list.csv): choose the file with the file chooser; the column picker shows the header and pre-selects the address column; the preview counts what
//      is malformed (with the reason in words), repeated and to be checked, and holds the EXACT credits (the preview's "Credits you have" is the dashboard's balance, the reserve is units x the price); confirm now;
//      the job page moves to "Finished" by itself; the results table gives each address its outcome WORD (the fixture DNS decides them); the filter narrows the page; "Download CSV" is the same results, exactly the
//      addresses of the list; the dashboard balance went down by the charge and the reserve came back; the job is in the jobs table
//   2  a file the platform would refuse is explained in words before anything is sent, and choosing another file puts it right
//   3  a list longer than one page of results: the pager, the second page, and the download that holds ALL of it
//
// What the stack makes of the fixture addresses is in web/e2e/README.md (module 1 answers from a fixture DNS; its vendor stand-in answers the two `valid+e2e...@provider.example.test` addresses). The first run of this
// file on the runner CONFIRMS those words; where a word below turns out different, the README's table is what to correct, not the assertion's strictness.
import { expect, test } from '@playwright/test'
import { t } from '../src/copy/index.ts'
import { csv, csvFile, fixturePath, fixtureText } from './support/data.ts'
import { escapeRegExp, fmt, parseCsv, runId } from './support/flows-data.ts'
import {
  chooseCsv,
  confirmNow,
  createVerifyJobFromCsv,
  downloadResults,
  goTo,
  h2,
  openAs,
  openConfirmedJob,
  readBalance,
  readResultRows,
  resultStateWord,
  resultsFileNamePattern,
  settled,
  stat,
  statNote,
  statNumber,
  unitCost,
  waitForJobStatus,
} from './support/flows-ui.ts'
import { shot } from './support/shots.ts'

const LIST = 'verify-list.csv'
const ADDRESS_RULE = /^[^@\s]+@[^@\s]+\.[^@\s]+$/
/** module 1's own price of one settled answer (internal/charge: m1_own); the vendor's is read from the preview (the reserve rate). */
const OWN_PRICE = 1

type Kind = 'malformed' | 'duplicate' | 'check'

/** What the platform makes of each row of a list: an address that is no address, a repeat of an earlier one (any letter case), or one to check. */
function kindsOf(addresses: string[]): Kind[] {
  const seen = new Set<string>()
  return addresses.map((a) => {
    if (!ADDRESS_RULE.test(a)) return 'malformed'
    const key = a.toLowerCase()
    if (seen.has(key)) return 'duplicate'
    seen.add(key)
    return 'check'
  })
}

interface Expected {
  /** the outcome's word in the table */
  outcome: string
  /** the detail line, when this spec asserts it */
  detail?: string
  /** the cost word, when this spec asserts it */
  cost?: string
}

/** The outcome module 1 gives an address from its fixture DNS (README: "the fixtures"), in the catalogue's words. `vendor`: the preview's price says the outside lookup is on for this user. */
function expectedFor(address: string, kind: Kind, vendor: boolean): Expected {
  if (kind === 'malformed') return { outcome: t('common.resultState.malformed'), detail: t('common.malformed.no_at'), cost: t('jobs.cost.free') }
  if (kind === 'duplicate') return { outcome: t('common.resultState.duplicate'), cost: t('jobs.cost.free') }
  const domain = address.slice(address.lastIndexOf('@') + 1).toLowerCase()
  if (domain === 'free.example.test') return { outcome: t('common.resultState.risky'), detail: t('common.reason.plausible'), cost: t('jobs.cost.free') }
  if (/^d[1-4]\.example\.test$/.test(domain)) return { outcome: t('common.resultState.invalid'), detail: t('common.reason.no_mx'), cost: t('jobs.cost.charged') }
  if (domain === 'provider.example.test') {
    // routed to the vendor stand-in, which says valid: "risky, confirmed by a second lookup", charged at the vendor's price. Without the vendor (an opted-out user) the word is still risky.
    return vendor
      ? { outcome: t('common.resultState.risky'), detail: t('common.reason.external_verified'), cost: t('jobs.cost.unit.m1_vendor') }
      : { outcome: t('common.resultState.risky') }
  }
  throw new Error('this spec has no expectation for an address of this domain')
}

test('a verify job from a CSV list: columns, preview, exact credits, confirm, results, download, balance, jobs table', async ({ page, request }) => {
  test.setTimeout(300_000)
  const addresses = parseCsv(fixtureText(LIST))
    .slice(1)
    .map((r) => r[0] ?? '')
  const kinds = kindsOf(addresses)
  const toCheck = kinds.filter((k) => k === 'check').length
  await openAs(page, request, 'a')

  const before = await test.step('the dashboard says how many credits the user has for verifications', async () => {
    const balance = await readBalance(page, 'm1')
    expect(balance.available, 'user a is funded at module 1').toBeGreaterThan(1_000)
    await settled(page, 'verify-dashboard-before')
    return balance
  })

  await test.step('the New verify job screen asks for a file and cannot go on without one', async () => {
    await goTo(page, 'newVerify')
    await expect(page.getByText(t('newjob.verify.intro'))).toBeVisible()
    await expect(page.getByRole('button', { name: t('newjob.verify.check'), exact: true })).toBeDisabled()
    await expect(page.getByText(t('newjob.verify.needFile'))).toBeVisible()
    await settled(page, 'verify-new-empty')
  })

  await test.step('the file is chosen with the file chooser; the column picker shows its header and pre-selects the address column', async () => {
    await chooseCsv(page, fixturePath(LIST))
    await expect(page.getByText(/^verify-list\.csv \(/)).toBeVisible()
    await expect(page.getByText(t('newjob.csv.found', { rows: fmt(addresses.length), columns: 3 }))).toBeVisible()

    const picker = page.getByLabel(t('newjob.columns.address'), { exact: true })
    await expect(picker).toHaveValue('0')
    expect(await picker.evaluate((el: HTMLSelectElement) => el.selectedOptions[0]?.textContent ?? ''), 'the pre-selected column is the header "email"').toBe('email')

    const sample = page.getByRole('table', { name: t('newjob.csv.sampleCaption') })
    await expect(sample.getByRole('columnheader')).toHaveText([`email (${t('newjob.columns.addressMark')})`, 'first_name', 'company'])
    await expect(sample.getByRole('row')).toHaveCount(1 + 5) // the header and the first five rows
    await expect(sample.getByRole('row').nth(1).getByRole('cell')).toHaveText(['ana@free.example.test', 'Ana', 'Free Co'])
    await expect(page.getByRole('list', { name: t('newjob.columns.mergeTitleVerify') }).getByRole('listitem')).toHaveText(['first_name', 'company'])
    await settled(page, 'verify-new-columns')
  })

  await test.step('choosing another column for the address moves the mark; a column the platform fills itself is refused in words; the first choice puts it right', async () => {
    const picker = page.getByLabel(t('newjob.columns.address'), { exact: true })
    await picker.selectOption({ label: 'company' })
    await expect(page.getByRole('table', { name: t('newjob.csv.sampleCaption') }).getByRole('columnheader').nth(2)).toHaveText(`company (${t('newjob.columns.addressMark')})`)
    // "email" is a name the platform fills in itself, so it cannot sit beside another address column
    await expect(page.getByText(t('newjob.problem.reservedColumn', { name: 'email' }))).toBeVisible()
    await expect(page.getByRole('button', { name: t('newjob.verify.check'), exact: true })).toBeDisabled()
    await picker.selectOption({ label: 'email' })
    await expect(page.getByText(t('newjob.problem.title'))).toHaveCount(0)
    await expect(page.getByRole('button', { name: t('newjob.verify.check'), exact: true })).toBeEnabled()
  })

  const charged = await test.step('the preview counts the list in words and holds the exact credits', async () => {
    await page.getByRole('button', { name: t('newjob.verify.check'), exact: true }).click()
    await expect(h2(page, t('newjob.preview.title'))).toBeVisible()

    expect(await statNumber(page, t('newjob.preview.rows')), 'rows in the list').toBe(addresses.length)
    expect(await statNumber(page, t('newjob.preview.malformed')), 'rows that are no address').toBe(kinds.filter((k) => k === 'malformed').length)
    await expect(statNote(page, t('newjob.preview.malformed'))).toContainText(t('common.malformed.no_at'))
    expect(await statNumber(page, t('newjob.preview.duplicates')), 'repeated addresses').toBe(kinds.filter((k) => k === 'duplicate').length)
    expect(await statNumber(page, t('newjob.preview.unitsVerify')), 'addresses that will be checked').toBe(toCheck)

    const unit = await unitCost(page, 'check')
    expect([1, 2], 'the price of one address is the own price, or the outside lookup\'s when it is on').toContain(unit)
    expect(await statNumber(page, t('newjob.preview.credits')), 'credits held = addresses to check x the price').toBe(toCheck * unit)
    expect(await statNumber(page, t('newjob.preview.available')), 'the credits the preview says the user has are the dashboard\'s').toBe(before.available)
    await expect(stat(page, t('newjob.preview.shortfall'))).toHaveCount(0)

    // the line about the outside service (placeholder words) and the way to the setting
    await expect(page.getByText(t('newjob.disclosure.vendor')).or(page.getByText(t('newjob.disclosure.vendorOptedOut')))).toBeVisible()
    await expect(page.getByRole('link', { name: t('newjob.disclosure.settingsLink'), exact: true })).toBeVisible()

    // "start now" is the default, and the confirm is open
    await expect(page.getByRole('radio', { name: t('newjob.confirm.now'), exact: true })).toBeChecked()
    await expect(page.getByRole('radio', { name: t('newjob.confirm.later'), exact: true })).not.toBeChecked()
    await expect(page.getByRole('button', { name: t('newjob.confirm.submitNow'), exact: true })).toBeEnabled()
    await settled(page, 'verify-new-preview')
    return { unit }
  })

  const jobId = await test.step('confirm now: the confirmation says how many addresses and how many credits', async () => {
    const id = await confirmNow(page)
    await expect(page.getByText(t('newjob.done.pending', { count: fmt(toCheck) }))).toBeVisible()
    await expect(page.getByText(t('newjob.done.reserved', { credits: fmt(toCheck * charged.unit) }))).toBeVisible()
    await settled(page, 'verify-new-confirmed')
    return id
  })

  await test.step('the job page polls until the job is finished', async () => {
    await openConfirmedJob(page)
    await waitForJobStatus(page, jobId, 'succeeded')
    await expect(page.getByText(t('jobs.now.succeeded'))).toBeVisible()
  })

  const vendor = charged.unit > OWN_PRICE
  await test.step('the results table gives every address its outcome, as module 1\'s fixture DNS decides it', async () => {
    await expect(page.getByTestId(/^results-row-/)).toHaveCount(addresses.length)
    const shown = await readResultRows(page)
    expect(shown.map((r) => r.ordinal), 'one result for each row of the list, by its position').toEqual(addresses.map((_, i) => i + 1))
    for (const [i, address] of addresses.entries()) {
      const row = shown[i]!
      const want = expectedFor(address, kinds[i]!, vendor)
      expect(row.address.toLowerCase() === address.toLowerCase(), `row ${i + 1}: the result is for the address of that position in the list`).toBe(true)
      expect(row.outcome, `row ${i + 1}: the outcome`).toBe(want.outcome)
      if (want.cost !== undefined) expect(row.cost, `row ${i + 1}: the cost`).toBe(want.cost)
      if (want.detail !== undefined) expect(row.details, `row ${i + 1}: the detail line`).toContain(want.detail)
    }
    await settled(page, 'verify-job-finished')
  })

  await test.step('the outcome filter narrows the page to one outcome and back', async () => {
    const filter = page.getByLabel(t('jobs.results.filter.label'), { exact: true })
    await filter.selectOption({ label: t('common.resultState.invalid') })
    await expect(page.getByTestId(/^results-row-/)).toHaveCount(addresses.filter((a) => /@d[1-4]\.example\.test$/i.test(a)).length)
    for (const row of await readResultRows(page)) expect(row.outcome).toBe(t('common.resultState.invalid'))
    await shot(page, 'verify-job-results-filtered')
    await filter.selectOption({ value: '' })
    await expect(page.getByTestId(/^results-row-/)).toHaveCount(addresses.length)
  })

  const charge = await test.step('"Download CSV" is the same results: exactly the addresses of the list, each with its outcome', async () => {
    const shown = await readResultRows(page)
    const file = await downloadResults(page)
    expect(file.filename, 'the file is named for the job, its kind and its day').toMatch(resultsFileNamePattern('m1', jobId))
    await expect(page.getByText(t('jobs.csv.ready'))).toBeVisible()

    const [header, ...lines] = parseCsv(file.text)
    expect(header, 'the columns of the results file').toEqual(['ordinal', 'email', 'origin', 'state', 'reason', 'decision', 'unit', 'rule', 'suppressed'])
    const at = (name: string) => (header ?? []).indexOf(name)
    expect(lines.length, 'one line for each row of the list').toBe(addresses.length)
    expect(lines.map((l) => Number(l[at('ordinal')])), 'the lines are in the list\'s order').toEqual(addresses.map((_, i) => i + 1))
    expect(
      lines.map((l) => (l[at('email')] ?? '').toLowerCase()),
      'exactly the addresses of the list',
    ).toEqual(addresses.map((a) => a.toLowerCase()))
    // the file and the table are one result: the same outcome for every row, and the lines that were charged are the ones the table says were
    expect(
      lines.map((l) => resultStateWord(l[at('state')] ?? '')),
      'the outcome of every line is the table\'s',
    ).toEqual(shown.map((r) => r.outcome))
    expect(lines.filter((l) => l[at('decision')] === 'charge').length, 'lines charged').toBe(shown.filter((r) => r.cost.startsWith(t('jobs.cost.charged'))).length)
    expect(lines.every((l) => l[at('suppressed')] === 'false'), 'no address of the list is on the suppression list').toBe(true)

    // what the job cost: one own price for each charged answer, the vendor's price for each that the outside lookup gave
    return lines.filter((l) => l[at('decision')] === 'charge').reduce((sum, l) => sum + (l[at('unit')] === 'm1_vendor' ? charged.unit : OWN_PRICE), 0)
  })

  await test.step('the dashboard balance went down by the charge, and the credits set aside came back', async () => {
    await goTo(page, 'dashboard')
    await expect
      .poll(
        async () => {
          const now = await readBalance(page, 'm1')
          return `charged ${before.available - now.available}, set aside changed by ${now.reserved - before.reserved}`
        },
        { message: 'the balance after the job', timeout: 60_000, intervals: [2_000] },
      )
      .toBe(`charged ${charge}, set aside changed by 0`)
    await expect(page.getByTestId(`job-row-${jobId}`)).toBeVisible() // "Recent results"
    await expect(page.getByTestId(`job-status-${jobId}`)).toHaveText(t('common.jobStatus.succeeded'))
    await settled(page, 'verify-dashboard-after')
  })

  await test.step('the same job is in the jobs table', async () => {
    await goTo(page, 'jobs')
    const row = page.getByTestId(`job-row-${jobId}`)
    await expect(row).toBeVisible()
    await expect(row.getByTestId(`job-status-${jobId}`)).toHaveText(t('common.jobStatus.succeeded'))
    await expect(row).toContainText(t('jobs.count.other', { count: fmt(addresses.length) }))
    await expect(row.getByRole('link', { name: new RegExp(`^${escapeRegExp(t('jobs.kind.m1'))} `) })).toBeVisible()
    await settled(page, 'verify-jobs-table')
  })
})

test('a file the platform would refuse is explained in words before anything is sent, and another file puts it right', async ({ page, request }) => {
  await openAs(page, request, 'a')
  await goTo(page, 'newVerify')

  await test.step('a file with two columns of one name', async () => {
    await chooseCsv(page, csvFile('twice.csv', csv(['email', 'first_name', 'first_name'], [['one@free.example.test', 'A', 'B']])))
    const alert = page.getByRole('alert').filter({ hasText: t('newjob.problem.title') })
    await expect(alert).toContainText(t('newjob.problem.repeatedColumn', { name: 'first_name' }))
    await expect(page.getByRole('button', { name: t('newjob.verify.check'), exact: true })).toBeDisabled()
    await expect(page.getByText(t('newjob.verify.fixProblems'))).toBeVisible()
    await settled(page, 'verify-new-refused')
  })

  await test.step('another file: the problem goes and the list can be checked', async () => {
    await page.getByRole('button', { name: t('newjob.csv.change'), exact: true }).click()
    await chooseCsv(page, csvFile('fine.csv', csv(['email', 'first_name'], [['one@free.example.test', 'A']])))
    await expect(page.getByText(t('newjob.problem.title'))).toHaveCount(0)
    await expect(page.getByRole('button', { name: t('newjob.verify.check'), exact: true })).toBeEnabled()
  })
})

test('a list longer than one page of results: the pager, the second page, and a download that holds all of it', async ({ page, request }) => {
  test.setTimeout(420_000)
  const run = runId()
  // 120 distinct addresses over the fixture domains (no mail server: invalid; one with one: risky), so the results are two pages of 100 and 20
  const domains = ['d1.example.test', 'd2.example.test', 'd3.example.test', 'd4.example.test', 'free.example.test']
  const addresses = Array.from({ length: 120 }, (_, i) => `page-${run}-${String(i + 1).padStart(3, '0')}@${domains[i % domains.length]}`)
  await openAs(page, request, 'a')

  const jobId = await createVerifyJobFromCsv(page, csvFile(`paging-${run}.csv`, csv(['email'], addresses.map((a) => [a]))))
  await openConfirmedJob(page)
  await waitForJobStatus(page, jobId, 'succeeded', 360_000)

  await test.step('the first page has 100 results and a pager', async () => {
    await expect(page.getByTestId(/^results-row-/)).toHaveCount(100)
    await expect(page.getByText(t('common.pagination.page', { n: 1 }))).toBeVisible()
    await expect(page.getByRole('button', { name: t('common.pagination.previous'), exact: true })).toBeDisabled()
    await expect(page.getByRole('button', { name: t('common.pagination.next'), exact: true })).toBeEnabled()
    const rows = await readResultRows(page)
    expect([rows[0]?.ordinal, rows[99]?.ordinal], 'the first page holds the rows 1 to 100').toEqual([1, 100])
    expect(rows[0]?.address.toLowerCase()).toBe(addresses[0])
    await settled(page, 'verify-results-page-1')
  })

  await test.step('the second page has the other 20, and the pager goes back', async () => {
    await page.getByRole('button', { name: t('common.pagination.next'), exact: true }).click()
    await expect(page.getByText(t('common.pagination.page', { n: 2 }))).toBeVisible()
    await expect(page.getByTestId(/^results-row-/)).toHaveCount(20)
    const rows = await readResultRows(page)
    expect([rows[0]?.ordinal, rows[19]?.ordinal], 'the second page holds the rows 101 to 120').toEqual([101, 120])
    expect(rows[19]?.address.toLowerCase()).toBe(addresses[119])
    await expect(page.getByRole('button', { name: t('common.pagination.next'), exact: true })).toBeDisabled()
    await shot(page, 'verify-results-page-2')
    await page.getByRole('button', { name: t('common.pagination.previous'), exact: true }).click()
    await expect(page.getByText(t('common.pagination.page', { n: 1 }))).toBeVisible()
    await expect(page.getByTestId(/^results-row-/)).toHaveCount(100)
  })

  await test.step('the download is the whole job, not the page', async () => {
    const file = await downloadResults(page)
    const [, ...lines] = parseCsv(file.text)
    expect(lines.length, 'one line for each address of the list').toBe(addresses.length)
    expect(new Set(lines.map((l) => (l[1] ?? '').toLowerCase())).size, 'each address once').toBe(addresses.length)
    expect(lines.map((l) => (l[1] ?? '').toLowerCase()), 'exactly the addresses of the list, in its order').toEqual(addresses)
  })
})

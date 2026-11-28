// Module 3 sender accounts for the specs of chunks A10 and A11: made without a screen (a spec that only needs a module 3 account to send through), read, removed, and the screen's own form filled. The
// stage's rules (web/e2e/README.md, "Sending: modules 2 and 3"): host `E2E_M3_SINK_HOST`, port 1025, any username and secret (the sink accepts every credential), a From address below `example.test`
// (aligned: the From-gate runs in ENFORCE against a fixture DNS that answers for example.test) or below a name outside the fixture, such as `unaligned.test` (not aligned: a warning to acknowledge).
//
// The SMTP password of an account is made here per call (random, never printed): a spec asserts that no screen holds it, by a boolean, and never shows it.
import { randomBytes } from 'node:crypto'
import { expect, type APIRequestContext, type Locator, type Page } from '@playwright/test'
import type { M3Account } from '../../src/api/types.ts'
import { t } from '../../src/copy/index.ts'
import { escapeRe } from './cross-ui.ts'
import { errorCodeOf, sleep } from './cross-api.ts'
import { m3SinkHost } from './env.ts'
import { bearer, type Session } from './users.ts'

/** A From domain the fixture DNS answers aligned for (the sink's own name is below it). */
export const ALIGNED_DOMAIN = 'example.test'
/** A From domain outside the fixture: module 3's From-gate answers "unaligned, warned" and wants the warning acknowledged. */
export const UNALIGNED_DOMAIN = 'unaligned.test'

/** A password for a throw-away account: random, 24 hex digits. */
export const accountSecret = (): string => randomBytes(12).toString('hex')

export interface NewAccount {
  smtp_host: string
  smtp_port: number
  smtp_username: string
  smtp_secret: string
  from_address: string
  from_name?: string
}

/** An account at the sink: `tag` makes the username and the From local part unique to the spec and the run. */
export function accountAt(tag: string, domain: string = ALIGNED_DOMAIN, secret: string = accountSecret()): NewAccount {
  return { smtp_host: m3SinkHost(), smtp_port: 1025, smtp_username: `e2e-${tag}`, smtp_secret: secret, from_address: `${tag}@${domain}`, from_name: `E2E ${tag}` }
}

// ---- through the API ----

export async function listAccounts(request: APIRequestContext, s: Session): Promise<M3Account[]> {
  const out: M3Account[] = []
  let cursor: string | undefined
  for (let page = 0; page < 50; page++) {
    const res = await request.get('/v1/m3/accounts', { headers: bearer(s), params: cursor === undefined ? {} : { cursor } })
    if (res.status() !== 200) throw new Error(`GET the module 3 accounts answered ${res.status()}`)
    const body = (await res.json()) as { accounts: M3Account[]; next_cursor: string | null }
    out.push(...body.accounts)
    if (body.next_cursor === null) return out
    cursor = body.next_cursor
  }
  throw new Error('the list of accounts did not end after 50 pages')
}

/** Adds an account (acknowledging the From-gate warning when `acknowledge`); returns it. */
export async function addAccount(request: APIRequestContext, s: Session, account: NewAccount, acknowledge = false): Promise<M3Account> {
  const res = await request.post('/v1/m3/accounts', { headers: bearer(s), data: { ...account, ...(acknowledge ? { acknowledge_from_gate: true } : {}) } })
  if (res.status() !== 201) throw new Error(`POST an account answered ${res.status()}, code ${(await errorCodeOf(res)) ?? 'none'}`)
  return ((await res.json()) as { account: M3Account }).account
}

/** Removes an account; an account that is already gone is fine. */
export async function removeAccount(request: APIRequestContext, s: Session, id: string): Promise<void> {
  const res = await request.post(`/v1/m3/accounts/${id}/remove`, { headers: bearer(s) })
  if (res.status() !== 200 && res.status() !== 404) throw new Error(`POST an account removal answered ${res.status()}`)
}

/** Removes every account of the user whose From address starts with `prefix` (the spec's own, found by its tag): a cleanup that must not take another spec's account. */
export async function removeAccountsStartingWith(request: APIRequestContext, s: Session, prefix: string): Promise<void> {
  try {
    for (const a of await listAccounts(request, s)) if (a.from_address.startsWith(prefix)) await removeAccount(request, s, a.id)
  } catch {
    // best effort: the stack is thrown away when the run ends
  }
}

/** An aligned account, added through the API and waited for until its login is verified (the sender can route through it). */
export async function addReadyAccount(request: APIRequestContext, s: Session, tag: string): Promise<M3Account> {
  const added = await addAccount(request, s, accountAt(tag))
  const deadline = Date.now() + 60_000
  for (;;) {
    const found = (await listAccounts(request, s)).find((a) => a.id === added.id)
    if (found?.verification === 'verified' && found.status === 'active') return found
    if (found?.verification === 'failed') throw new Error('the login of the new account failed: the sink did not accept it')
    if (Date.now() > deadline) throw new Error(`the new account is still ${found?.verification ?? 'missing'} after 60 s`)
    await sleep(1000)
  }
}

// ---- the add form ----

const optionalLabel = (label: string): RegExp => new RegExp(`^${escapeRe(label)}`)

/** The fields of the add form, by label (an optional field's label is followed by "(optional)"). */
export function addForm(page: Page): Record<'host' | 'port' | 'username' | 'password' | 'fromAddress' | 'fromName' | 'dailyLimit', Locator> {
  return {
    host: page.getByLabel(t('accounts.add.host'), { exact: true }),
    port: page.getByLabel(optionalLabel(t('accounts.add.port'))),
    username: page.getByLabel(t('accounts.add.username'), { exact: true }),
    password: page.getByLabel(t('accounts.add.password'), { exact: true }),
    fromAddress: page.getByLabel(t('accounts.add.fromAddress'), { exact: true }),
    fromName: page.getByLabel(optionalLabel(t('accounts.add.fromName'))),
    dailyLimit: page.getByLabel(optionalLabel(t('accounts.add.dailyLimit'))),
  }
}

/** Opens the add form on the accounts page. */
export async function openAddForm(page: Page): Promise<void> {
  await page.getByRole('button', { name: t('accounts.add.open'), exact: true }).click()
  await expect(page.getByRole('heading', { name: t('accounts.add.title'), exact: true })).toBeVisible()
}

/** Types an account into the open add form. */
export async function fillAddForm(page: Page, account: NewAccount): Promise<void> {
  const f = addForm(page)
  await f.host.fill(account.smtp_host)
  await f.port.fill(String(account.smtp_port))
  await f.username.fill(account.smtp_username)
  await f.password.fill(account.smtp_secret)
  await f.fromAddress.fill(account.from_address)
  if (account.from_name !== undefined) await f.fromName.fill(account.from_name)
}

/** Presses the submit button of the add form (whatever it says now) and resolves to the account the platform created, read from its 201 answer. */
export async function submitAdd(page: Page, label: 'accounts.add.submit' | 'accounts.add.submitAnyway'): Promise<M3Account> {
  const created = page.waitForResponse((r) => new URL(r.url()).pathname === '/v1/m3/accounts' && r.request().method() === 'POST' && r.status() === 201, { timeout: 60_000 })
  await page.getByRole('button', { name: t(label), exact: true }).click()
  return ((await (await created).json()) as { account: M3Account }).account
}

export const accountRow = (page: Page, id: string): Locator => page.getByTestId(`account-row-${id}`)

/** The sentences that a screen must never hold about an account: the words of DNS records and of the From-gate, and the name of a module. */
export const RAW_MODULE_TEXT = /\b(spf|dkim|dmarc|v=spf1|from_alignment|alignment_state)\b/i

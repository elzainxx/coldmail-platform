// The controllable SMTP sink (tests/harness/sink): every message the senders (modules 2 and 3) deliver ends here, and a spec reads what arrived and makes the next recipients bounce or wait. Its control API is plain
// http on the private network (E2E_SINK_URL, no authentication). The sink counts per recipient and keeps the raw bytes of every message.
//
//   GET  /api/messages           [{seq, at, to[], from, envelope_from, subject, message_id, size, held_ms}] oldest first
//   GET  /api/message?seq=N      the raw message (message/rfc822)
//   POST /api/mode               REPLACES the mode: {reject_rcpt[], reject_code, defer_rcpt[], data_delay_ms, delay_recipients[], fail_auth}; unknown fields are 400
//   POST /api/reset              forget every message and put the default mode back
//
// The sink is SHARED by the whole run (every spec, both senders): a spec that needs a bounce or a pause sets the mode for ITS recipients (addresses no other spec uses) and puts the default back when it is done
// (`sinkMode(request, {})`); a spec reads the sink through `messagesTo(request, address)`, never the whole list. Do not reset the sink in a spec: another spec's messages are in it.
import type { APIRequestContext } from '@playwright/test'
import { sinkUrl } from './env.ts'

export interface SinkMessage {
  seq: number
  at: string
  to: string[]
  from: string
  envelope_from: string
  subject: string
  message_id: string
  size: number
  held_ms: number
}

export interface SinkMode {
  /** Recipients the sink refuses at RCPT TO with `reject_code` (default 550): the sender records a bounce. */
  reject_rcpt?: string[]
  reject_code?: number
  /** Recipients the sink defers (451): the sender retries, then gives up (module 2: two attempts in this scope). */
  defer_rcpt?: string[]
  /** Milliseconds the sink holds the final reply of a message to a recipient of `delay_recipients`: a send stays in flight that long (the window a pause or a cancel needs). Both or neither. */
  data_delay_ms?: number
  delay_recipients?: string[]
  fail_auth?: boolean
}

export async function sinkMessages(request: APIRequestContext): Promise<SinkMessage[]> {
  const res = await request.get(`${sinkUrl()}/api/messages`)
  if (res.status() !== 200) throw new Error(`the sink answered ${res.status()} to GET /api/messages`)
  return (await res.json()) as SinkMessage[]
}

/** The messages delivered to ONE recipient (case-insensitive), oldest first. */
export async function messagesTo(request: APIRequestContext, address: string): Promise<SinkMessage[]> {
  const want = address.trim().toLowerCase()
  return (await sinkMessages(request)).filter((m) => m.to.some((t) => t.toLowerCase() === want))
}

/** The raw bytes of one message: its headers, then its parts. */
export async function rawMessage(request: APIRequestContext, seq: number): Promise<string> {
  const res = await request.get(`${sinkUrl()}/api/message`, { params: { seq } })
  if (res.status() !== 200) throw new Error(`the sink answered ${res.status()} to GET /api/message?seq=${seq}`)
  return await res.text()
}

/** Waits until `count` messages have been delivered to the recipient (the sender is asynchronous: a job's status moves before the last message lands) and returns them. */
export async function waitForMessages(request: APIRequestContext, address: string, count = 1, timeoutMs = 60_000): Promise<SinkMessage[]> {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const got = await messagesTo(request, address)
    if (got.length >= count) return got
    if (Date.now() > deadline) throw new Error(`${got.length} message(s) reached ${address} within ${timeoutMs} ms, ${count} expected`)
    await new Promise((r) => setTimeout(r, 500))
  }
}

/** Sets the sink's mode (it REPLACES the whole mode; `{}` is the default: accept everything at once). */
export async function sinkMode(request: APIRequestContext, mode: SinkMode): Promise<void> {
  const body = { reject_rcpt: [], defer_rcpt: [], delay_recipients: [], data_delay_ms: 0, fail_auth: false, ...mode }
  const res = await request.post(`${sinkUrl()}/api/mode`, { data: body })
  if (res.status() !== 200) throw new Error(`the sink answered ${res.status()} to POST /api/mode: it refused the mode (an unknown field, or an invalid value)`)
}

/** The header block of a raw message, unfolded (a folded header line is joined to its first line). */
export function headersOf(raw: string): string {
  const end = raw.search(/\r?\n\r?\n/)
  return (end < 0 ? raw : raw.slice(0, end)).replace(/\r?\n[ \t]+/g, ' ')
}

/** The value of the first header of that name, or undefined. */
export function headerOf(raw: string, name: string): string | undefined {
  const re = new RegExp(`^${name.replace(/[-]/g, '\\-')}:[ \\t]*(.*)$`, 'im')
  return re.exec(headersOf(raw))?.[1]?.trim()
}

/**
 * The unsubscribe link of a message: the https address in its `List-Unsubscribe` header (`<https://...>`), the one a mail client's button uses and the one the platform put in the message's footer. The
 * message must also carry `List-Unsubscribe-Post` (the one-click form): a message without it is not a message of the platform. Open the returned address in the BROWSER (`page.goto`): it is under the
 * platform's own https name, which the browser of the run can reach.
 */
export function unsubscribeLink(raw: string): string {
  const value = headerOf(raw, 'List-Unsubscribe')
  if (value === undefined) throw new Error('the message has no List-Unsubscribe header')
  const m = /<(https:\/\/[^>\s]+)>/.exec(value)
  if (m?.[1] === undefined) throw new Error('the List-Unsubscribe header holds no https address')
  if (headerOf(raw, 'List-Unsubscribe-Post') === undefined) throw new Error('the message has no List-Unsubscribe-Post header (the one-click form)')
  return m[1]
}

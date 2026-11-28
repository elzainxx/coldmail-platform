// What arrived at the sink, read as a person's mail client would: the headers, the decoded text and HTML parts, and the unsubscribe link. The senders (modules 2 and 3) build a multipart/alternative message whose
// parts may be 8bit, quoted-printable or base64 (module 2's variation toggles choose per message), so a spec never matches the raw bytes of a body: it reads the DECODED parts.
//
//   const [m] = await readSinkFor(request, address)        waits for the message(s) of ONE recipient and parses them
//   m.subject, m.html, m.text                             decoded
//   m.listUnsubscribeHeaders / m.listUnsubscribePostHeaders  how many of each header the message carries (the spec asks for exactly one of each)
//   m.unsubscribeUrl()                                    the https address in the List-Unsubscribe header (the sink helper's own reader)
//
// No value is ever put in an assertion message by these helpers: they return data, the specs compare booleans.
import type { APIRequestContext } from '@playwright/test'
import { headerOf, headersOf, messagesTo, rawMessage, unsubscribeLink, waitForMessages } from './sink.ts'

export interface MimePart {
  /** the part's Content-Type without parameters, lower case (`text/html`) */
  type: string
  /** the part's body, decoded from its transfer encoding and read as UTF-8 */
  body: string
}

export interface ReadMessage {
  seq: number
  raw: string
  /** the Subject header, unfolded and with any RFC 2047 words decoded */
  subject: string
  /** the To header as sent */
  to: string
  /** the From header as sent */
  from: string
  /** the decoded text/plain part ('' when the message has none) */
  text: string
  /** the decoded text/html part ('' when the message has none) */
  html: string
  /** how many List-Unsubscribe headers the message carries (it must be exactly one) */
  listUnsubscribeHeaders: number
  /** how many List-Unsubscribe-Post headers (exactly one) */
  listUnsubscribePostHeaders: number
  /** the value of List-Unsubscribe-Post */
  listUnsubscribePost: string | undefined
  /** the https address of List-Unsubscribe (it throws when there is none: read `listUnsubscribeHeaders` first when that is the question) */
  unsubscribeUrl: () => string
}

function quotedPrintable(body: string): Buffer {
  const joined = body.replace(/=\r?\n/g, '')
  const bytes: number[] = []
  for (let i = 0; i < joined.length; i++) {
    const c = joined.charAt(i)
    if (c === '=' && /^[0-9A-Fa-f]{2}$/.test(joined.slice(i + 1, i + 3))) {
      bytes.push(parseInt(joined.slice(i + 1, i + 3), 16))
      i += 2
    } else {
      bytes.push(...Buffer.from(c, 'utf8'))
    }
  }
  return Buffer.from(bytes)
}

/** A part's body decoded by its Content-Transfer-Encoding and read as UTF-8 (7bit and 8bit are as they are). */
export function decodeTransfer(body: string, encoding: string | undefined): string {
  switch ((encoding ?? '7bit').trim().toLowerCase()) {
    case 'quoted-printable':
      return quotedPrintable(body).toString('utf8')
    case 'base64':
      return Buffer.from(body.replace(/\s+/g, ''), 'base64').toString('utf8')
    default:
      return body
  }
}

/** RFC 2047 encoded words (`=?utf-8?B?...?=`, `=?utf-8?Q?...?=`) of a header value, decoded; a plain value is returned as it is. */
export function decodeWords(value: string): string {
  return value
    .replace(/(\?=)\s+(=\?)/g, '$1$2')
    .replace(/=\?([^?]+)\?([bBqQ])\?([^?]*)\?=/g, (_whole, _charset: string, kind: string, text: string) =>
      kind.toLowerCase() === 'b' ? Buffer.from(text, 'base64').toString('utf8') : quotedPrintable(text.replace(/_/g, ' ')).toString('utf8'),
    )
}

function splitEntity(entity: string): { head: string; body: string } {
  const m = /\r?\n\r?\n/.exec(entity)
  return m === null ? { head: entity, body: '' } : { head: entity.slice(0, m.index), body: entity.slice(m.index + m[0].length) }
}

function headerValue(head: string, name: string): string | undefined {
  const unfolded = head.replace(/\r?\n[ \t]+/g, ' ')
  return new RegExp(`^${name}:[ \\t]*(.*)$`, 'im').exec(unfolded)?.[1]?.trim()
}

/** Every leaf part of a message, depth first: a multipart is walked through its boundary, anything else is one part. */
export function mimeParts(entity: string): MimePart[] {
  const { head, body } = splitEntity(entity)
  const contentType = headerValue(head, 'Content-Type') ?? 'text/plain'
  const type = (contentType.split(';')[0] ?? 'text/plain').trim().toLowerCase()
  if (type.startsWith('multipart/')) {
    const boundary = /boundary="?([^";\s]+)"?/i.exec(contentType)?.[1]
    if (boundary === undefined) return []
    const delimiter = `--${boundary}`
    const out: MimePart[] = []
    for (const piece of body.split(delimiter).slice(1)) {
      if (piece.startsWith('--')) break // the closing delimiter
      out.push(...mimeParts(piece.replace(/^\r?\n/, '')))
    }
    return out
  }
  return [{ type, body: decodeTransfer(body, headerValue(head, 'Content-Transfer-Encoding')) }]
}

function count(raw: string, name: string): number {
  return headersOf(raw)
    .split(/\r?\n/)
    .filter((line) => line.toLowerCase().startsWith(`${name.toLowerCase()}:`)).length
}

/** A raw message of the sink, parsed. */
export function parseMessage(seq: number, raw: string): ReadMessage {
  const parts = mimeParts(raw)
  const unsubscribeHeaders = count(raw, 'List-Unsubscribe')
  return {
    seq,
    raw,
    subject: decodeWords(headerOf(raw, 'Subject') ?? ''),
    to: headerOf(raw, 'To') ?? '',
    from: headerOf(raw, 'From') ?? '',
    text: parts.find((p) => p.type === 'text/plain')?.body ?? '',
    html: parts.find((p) => p.type === 'text/html')?.body ?? '',
    listUnsubscribeHeaders: unsubscribeHeaders,
    listUnsubscribePostHeaders: count(raw, 'List-Unsubscribe-Post'),
    listUnsubscribePost: headerOf(raw, 'List-Unsubscribe-Post'),
    unsubscribeUrl: () => unsubscribeLink(raw),
  }
}

/**
 * The messages that reached ONE recipient, parsed, once `count` of them are there (the sender is asynchronous: a job's status moves before the last message lands). `timeoutMs` is generous on purpose:
 * module 2 sends with no pause in this scope and module 3 at most a second apart, but the job's own status is not the signal that the sink has the message.
 */
export async function readSinkFor(request: APIRequestContext, recipient: string, options: { count?: number; timeoutMs?: number } = {}): Promise<ReadMessage[]> {
  const got = await waitForMessages(request, recipient, options.count ?? 1, options.timeoutMs ?? 90_000)
  return Promise.all(got.map(async (m) => parseMessage(m.seq, await rawMessage(request, m.seq))))
}

/** How many messages the sink holds for a recipient right now (to say "no message reached it" after every other message of the job has arrived). */
export async function sinkCount(request: APIRequestContext, recipient: string): Promise<number> {
  return (await messagesTo(request, recipient)).length
}

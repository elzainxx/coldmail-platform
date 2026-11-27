// How a send job is made from what the screens hold. A list that is a CSV file goes up as the file itself, streamed, and everything else about the job (the
// message included) travels in the query, which the platform caps at 64 KiB in all (Docs/jobs-api.md, "Create a draft"). A message too long for that is said so
// BEFORE the file is sent, in words, instead of being refused after. A send to a finished verification has no file: its message goes in a JSON body.
import { jobsApi } from '../../api/endpoints.ts'
import { buildUrl } from '../../api/client.ts'
import type { CreateJobCsvQuery, JobEnvelope, UnknownChoice } from '../../api/types.ts'
import type { Message } from './authoring/message.ts'

/** The platform's cap on the query of a CSV create, and what is kept free of it for the parameters that are not the message. */
export const MAX_QUERY_CHARS = 64 * 1024
const HEADROOM = 1024

export type Create = (signal: AbortSignal) => Promise<JobEnvelope>

/** How long the query is once it is written into the address (percent-encoded). */
export function queryLength(query: CreateJobCsvQuery): number {
  const url = buildUrl('', '/v1/jobs', { ...query })
  const at = url.indexOf('?')
  return at === -1 ? 0 : url.length - at - 1
}

export function csvQuery(module: 'm2' | 'm3', emailColumn: string, message: Message): CreateJobCsvQuery {
  return {
    module,
    email_column: emailColumn,
    subject: message.subject,
    html: message.html,
    text: message.text,
    reply_to: message.replyTo.trim(),
  }
}

/** Does this message travel in a CSV create? */
export function fitsInQuery(query: CreateJobCsvQuery): boolean {
  return queryLength(query) <= MAX_QUERY_CHARS - HEADROOM
}

export function csvCreate(file: Blob, query: CreateJobCsvQuery): Create {
  return (signal) => jobsApi.createCsv(file, query, { signal })
}

/** A send to the results of a finished verification: no rows, the message and the answer about unknown addresses. */
export function chainedCreate(module: 'm2' | 'm3', sourceJobId: string, message: Message, unknownChoice: UnknownChoice | null): Create {
  const replyTo = message.replyTo.trim()
  return (signal) =>
    jobsApi.createJson(
      {
        module,
        source_job_id: sourceJobId,
        content: { subject: message.subject, html: message.html, text: message.text, ...(replyTo === '' ? {} : { reply_to: replyTo }) },
        ...(unknownChoice === null ? {} : { settings: { unknown_choice: unknownChoice } }),
      },
      { signal },
    )
}

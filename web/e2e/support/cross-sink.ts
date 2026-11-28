// Reading the sink for a GROUP of recipients (the ones of one spec): how many of them have a message, which ones, and when the count stopped moving. The sink is shared by the whole run, so a spec never
// reads the whole list (support/sink.ts says so): every function here filters to the recipients it is given. Addresses are compared lower case.
import type { APIRequestContext } from '@playwright/test'
import { sleep } from './cross-api.ts'
import { sinkMessages, type SinkMessage } from './sink.ts'

/** The messages the sink holds for any of `addresses`, oldest first. */
export async function messagesForAny(request: APIRequestContext, addresses: readonly string[]): Promise<SinkMessage[]> {
  const want = new Set(addresses.map((a) => a.trim().toLowerCase()))
  return (await sinkMessages(request)).filter((m) => m.to.some((to) => want.has(to.toLowerCase())))
}

/** How many messages the sink holds for the group. */
export async function countFor(request: APIRequestContext, addresses: readonly string[]): Promise<number> {
  return (await messagesForAny(request, addresses)).length
}

/** The addresses of the group that have at least one message (lower case). */
export async function recipientsReached(request: APIRequestContext, addresses: readonly string[]): Promise<Set<string>> {
  const reached = new Set<string>()
  const want = new Set(addresses.map((a) => a.trim().toLowerCase()))
  for (const m of await messagesForAny(request, addresses)) for (const to of m.to) if (want.has(to.toLowerCase())) reached.add(to.toLowerCase())
  return reached
}

/**
 * Waits until the group's count has not moved for `quietMs` and returns it. This is how a spec proves "nothing new is sent" (a pause) and "nothing more will come" (after a cancel): a message in
 * flight is held by the sink for its delay, so a quiet period longer than that delay is the proof. Gives up (throws) after `maxMs`.
 */
export async function settledCount(request: APIRequestContext, addresses: readonly string[], quietMs: number, maxMs = 90_000): Promise<number> {
  const deadline = Date.now() + maxMs
  let last = await countFor(request, addresses)
  let since = Date.now()
  while (Date.now() - since < quietMs) {
    if (Date.now() > deadline) throw new Error(`the sink's count for the group kept moving for ${maxMs} ms (it was ${last} at the end)`)
    await sleep(500)
    const now = await countFor(request, addresses)
    if (now !== last) {
      last = now
      since = Date.now()
    }
  }
  return last
}

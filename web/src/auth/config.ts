// /config.json: the public values the browser needs to talk to the sign-in service, served by the platform (PLATFORM_PUBLIC_SUPABASE_URL and
// PLATFORM_PUBLIC_SUPABASE_ANON_KEY). Fetched once at start. Nothing secret is in it: the anon key is public by design.
import type { AppConfig } from '../api/types.ts'

export class ConfigError extends Error {
  constructor() {
    super('config')
    this.name = 'ConfigError'
  }
}

export function parseConfig(value: unknown): AppConfig {
  const v = typeof value === 'object' && value !== null ? (value as Record<string, unknown>) : {}
  const url = v['supabase_url']
  const key = v['anon_key']
  if (typeof url !== 'string' || typeof key !== 'string' || key === '') throw new ConfigError()
  try {
    const u = new URL(url)
    if (u.protocol !== 'https:' && u.protocol !== 'http:') throw new ConfigError()
  } catch {
    throw new ConfigError()
  }
  return { supabase_url: url, anon_key: key }
}

let cached: Promise<AppConfig> | null = null

/** Fetch /config.json once (a failure is not cached, so a retry asks again). */
export function loadConfig(fetchImpl?: typeof fetch): Promise<AppConfig> {
  if (cached !== null) return cached
  const doFetch = fetchImpl ?? globalThis.fetch.bind(globalThis)
  const run = (async () => {
    let res: Response
    try {
      res = await doFetch('/config.json', { cache: 'no-store', credentials: 'omit', headers: { Accept: 'application/json' } })
    } catch {
      throw new ConfigError()
    }
    if (!res.ok) throw new ConfigError()
    let body: unknown
    try {
      body = await res.json()
    } catch {
      throw new ConfigError()
    }
    return parseConfig(body)
  })()
  cached = run
  run.catch(() => {
    if (cached === run) cached = null
  })
  return run
}

/** For tests: forget the cached answer. */
export function resetConfigCache(): void {
  cached = null
}

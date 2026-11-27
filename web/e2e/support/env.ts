// The environment the harness's e2e scope gives the browser's container (tests/harness/docker-compose.e2e.yml, service e2e-runner). Everything is read when it is needed, never at import, so that
// `playwright test --list` (the `web` workflow's check that the specs still load) works with none of it. A missing variable is a thrown error that names it: a spec must never run against a guessed address.
import { join } from 'node:path'

/** The value of a required variable; throws, naming it, when it is missing or blank. */
export function need(name: string): string {
  const v = process.env[name]
  if (v === undefined || v.trim() === '') {
    throw new Error(`${name} is not set: the specs run inside the harness's e2e-runner container (tests/harness/docker-compose.e2e.yml), which sets it`)
  }
  return v.trim()
}

const noSlash = (v: string): string => v.replace(/\/+$/, '')

/** The platform's origin as the browser sees it: the app, `/config.json`, `/v1/...` and `/u/<token>` are all under it (one origin, decision 1). */
export const appUrl = (): string => noSlash(need('E2E_BASE_URL'))

/** The address the browser calls GoTrue at (`/config.json`'s `supabase_url`, the issuer of every token): its routes are `${authUrl()}/token`, `/user`, `/verify`, `/health`. */
export const authUrl = (): string => noSlash(need('E2E_GOTRUE_URL'))

/** GoTrue's origin (what the Content-Security-Policy names in connect-src). */
export const authOrigin = (): string => new URL(authUrl()).origin

/** The public anon key: the only key the browser is ever given (it is in `/config.json`). It is a JWT: never put it, or a token, in an assertion message. */
export const anonKey = (): string => need('E2E_ANON_KEY')

/** The controllable sink's control API (`GET /api/messages`, `GET /api/message?seq=N`, `POST /api/mode`, `POST /api/reset`): plain http, on the private network only. */
export const sinkUrl = (): string => noSlash(need('E2E_SINK_URL'))

/** The host name a module 3 account names for the sink (private-looking, below the From domain example.test: the account verifies and is aligned). The port is 1025. */
export const m3SinkHost = (): string => need('E2E_M3_SINK_HOST')

/** The directory the run's files are written to (the workflow uploads it). */
export const artifactsDir = (): string => process.env['E2E_ARTIFACTS_DIR'] ?? join(process.cwd(), 'e2e-artifacts')

/** The directory the specs' pictures are written to: one PNG per main screen (web/e2e/support/shots.ts). */
export const shotsDir = (): string => process.env['E2E_SHOTS_DIR'] ?? join(artifactsDir(), 'shots')

/** Where the stage wrote the users (web/e2e/support/users.ts reads it). */
export const fixturesPath = (): string => need('E2E_FIXTURES')

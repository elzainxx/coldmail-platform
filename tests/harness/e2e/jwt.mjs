// jwt.mjs — mints one HS256 token for the e2e scope: the anon key the web app is given (role anon) and the service-role key the stage calls GoTrue's admin routes with (role service_role).
//
//   E2E_JWT_SECRET=<GoTrue's secret> node jwt.mjs <role> <ttl-seconds>      prints the compact token and a newline, nothing else
//
// The secret is read from the ENVIRONMENT, never from an argument (an argument is visible to every process of the host). The claims are the ones the harness's own GoTrue stage proved GoTrue accepts for
// its admin routes (tests/harness/gotrue_test.go gtrServiceRoleKey): role, iss "supabase", iat, exp. A Supabase project's anon key is the same kind of token with the role anon.
// Exit status: 0 printed a token; 2 usage (no secret, an unknown role, a ttl that is not a whole number of seconds from 60 to 31536000).
import { createHmac } from 'node:crypto'

const ROLES = ['anon', 'service_role']
const [role, ttlText] = process.argv.slice(2)
const secret = process.env.E2E_JWT_SECRET ?? ''
const ttl = Number(ttlText)

if (secret.length < 32) {
  console.error('jwt.mjs: E2E_JWT_SECRET is missing or shorter than 32 characters')
  process.exit(2)
}
if (!ROLES.includes(role ?? '')) {
  console.error(`jwt.mjs: the role must be one of ${ROLES.join(', ')} (got '${role ?? ''}')`)
  process.exit(2)
}
if (!Number.isInteger(ttl) || ttl < 60 || ttl > 31536000) {
  console.error('jwt.mjs: the ttl must be a whole number of seconds from 60 to 31536000')
  process.exit(2)
}

const b64 = (v) => Buffer.from(v).toString('base64url')
const now = Math.floor(Date.now() / 1000)
const head = b64(JSON.stringify({ alg: 'HS256', typ: 'JWT' }))
const body = b64(JSON.stringify({ role, iss: 'supabase', iat: now, exp: now + ttl }))
const sig = createHmac('sha256', secret).update(`${head}.${body}`).digest('base64url')
process.stdout.write(`${head}.${body}.${sig}\n`)

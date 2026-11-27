// fixtures.mjs — writes the fixtures file the specs read (stdout), from what the stage made, and refuses to write one that is not what the specs rely on.
//
//   E2E_TAGS="a b c s" E2E_APP_URL=... E2E_AUTH_URL=... E2E_ISSUER=... E2E_SINK_URL=... E2E_M3_SINK_HOST=... E2E_SINK_SMTP_PORT=1025 \
//   E2E_ID_a=<uuid> E2E_EMAIL_a=<address> E2E_PW_a=<password> E2E_LINK_a=<invite link> E2E_FUNDED_a=1|0 E2E_M2ACCOUNT_a=<id or empty> ... node fixtures.mjs > fixtures.json
//
// The values come through the ENVIRONMENT of this one process, never through arguments (an argument is visible to every process of the host), and nothing is printed but the document itself: a refusal
// names the variable and the rule, never a value. The stage writes the document with `umask 077` (mode 600) into a directory the runner container mounts read-only; the document holds passwords and invite
// links of THIS run's throw-away stack, and web/e2e/README.md says so.
//
// The shape (the spec helpers, web/e2e/support/users.ts, read exactly this):
//   { "version": 1, "urls": { "app", "auth", "issuer", "sink", "m3_sink_host", "sink_smtp_port" },
//     "users": { "<tag>": { "tag", "id", "email", "password", "invite_link", "funded", "m2_account_id" } } }
// Exit status: 0 wrote the document; 1 a variable is missing or breaks its rule; 2 usage.
const env = process.env
const problems = []
const bad = (name, rule) => problems.push(`${name}: ${rule}`)
const need = (name) => {
  const v = env[name]
  if (v === undefined || v === '') {
    bad(name, 'is not set')
    return ''
  }
  return v
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/
const EMAIL = /^[a-z0-9][a-z0-9._+-]*@[a-z0-9.-]+\.[a-z]+$/
const tags = (env.E2E_TAGS ?? '').split(/\s+/).filter(Boolean)
if (tags.length === 0) {
  console.error('fixtures.mjs: E2E_TAGS names no user')
  process.exit(2)
}

const url = (name, scheme) => {
  const v = need(name)
  if (v && !new RegExp(`^${scheme}://[a-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$`).test(v)) bad(name, `must be a ${scheme} address without a query, a fragment or credentials`)
  return v.replace(/\/+$/, '')
}
const urls = {
  app: url('E2E_APP_URL', 'https'),
  auth: url('E2E_AUTH_URL', 'https'),
  issuer: url('E2E_ISSUER', 'https'),
  sink: url('E2E_SINK_URL', 'http'),
  m3_sink_host: need('E2E_M3_SINK_HOST'),
  sink_smtp_port: Number(need('E2E_SINK_SMTP_PORT')),
}
if (!Number.isInteger(urls.sink_smtp_port) || urls.sink_smtp_port < 1 || urls.sink_smtp_port > 65535) bad('E2E_SINK_SMTP_PORT', 'must be a port number')

const users = {}
const seen = { id: new Set(), email: new Set(), password: new Set() }
for (const tag of tags) {
  if (!/^[a-z][a-z0-9]*$/.test(tag)) {
    bad('E2E_TAGS', `the tag '${tag}' must be lower case letters and digits, starting with a letter`)
    continue
  }
  const id = need(`E2E_ID_${tag}`)
  const email = need(`E2E_EMAIL_${tag}`)
  const password = need(`E2E_PW_${tag}`)
  const link = need(`E2E_LINK_${tag}`)
  const funded = need(`E2E_FUNDED_${tag}`)
  if (id && !UUID.test(id)) bad(`E2E_ID_${tag}`, 'must be a lower-case uuid')
  if (email && !EMAIL.test(email)) bad(`E2E_EMAIL_${tag}`, 'must be a lower-case address')
  if (password && password.length < 16) bad(`E2E_PW_${tag}`, 'must be at least 16 characters (a minted password, never a literal)')
  if (link && !(link.startsWith(`${urls.issuer}/verify?`) && link.includes('type=invite'))) bad(`E2E_LINK_${tag}`, `must be an invite link under ${urls.issuer}/verify (GoTrue did not take API_EXTERNAL_URL, or the link is not an invite)`)
  if (funded && funded !== '1' && funded !== '0') bad(`E2E_FUNDED_${tag}`, 'must be 1 or 0')
  for (const [kind, v] of [['id', id], ['email', email], ['password', password]]) {
    if (v && seen[kind].has(v)) bad(`E2E_${kind === 'password' ? 'PW' : kind.toUpperCase()}_${tag}`, `equals another user's ${kind}: the users must be different`)
    if (v) seen[kind].add(v)
  }
  const account = env[`E2E_M2ACCOUNT_${tag}`] ?? ''
  if (funded === '1' && account === '') bad(`E2E_M2ACCOUNT_${tag}`, 'is empty for a funded user: module 2 has no sending account of this user')
  if (funded === '0' && account !== '') bad(`E2E_M2ACCOUNT_${tag}`, 'is set for a user who is not funded')
  users[tag] = { tag, id, email, password, invite_link: link, funded: funded === '1', m2_account_id: account === '' ? null : account }
}
if (!tags.some((t) => users[t]?.funded) || !tags.some((t) => users[t] && !users[t].funded)) bad('E2E_FUNDED_*', 'the fixtures need at least one funded user and one user without credits (the second is what the error-state specs sign in as)')

if (problems.length > 0) {
  for (const p of problems) console.error(`fixtures.mjs: ${p}`)
  process.exit(1)
}
process.stdout.write(`${JSON.stringify({ version: 1, urls, users }, null, 2)}\n`)

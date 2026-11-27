// Post-build check: the built page must hold nothing the platform's Content-Security-Policy refuses.
//
//   npm run build && node scripts/check-csp.mjs            (npm run check:csp, once package.json names it: see web/README.md)
//   node scripts/check-csp.mjs --selftest                  the instrument's own cases (every reject case names the reason it fails for)
//
// The platform serves the app with `script-src 'self'` and `default-src 'none'` (Docs/phase-6-spec.md, decision 1): an INLINE script or an inline event
// handler would not run, so a build that holds one is broken, and a build that needed `'unsafe-inline'` to work would weaken the policy. This check reads
// dist/index.html and fails on:
//   - a <script> with a body (inline script) or without a src
//   - a <script src> or <link rel=stylesheet|modulepreload href> that is not a path of this origin (script-src 'self', style-src 'self')
//   - an on*= attribute on any tag (onclick, onload, onerror...)
//   - a javascript: URL in href, src or action
//
// Exit codes: 0 = clean, 1 = a violation, 2 = could not run the check (no built index.html, or it holds no script at all: a page with no script is not
// the app, and a check that looked at nothing must not pass). The codes differ so a control run that exits 2 cannot pass for "correctly rejected".

import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

const TAG = /<(\/?)([a-zA-Z][^\s/>]*)((?:"[^"]*"|'[^']*'|[^>"'])*)>/g
const ATTR = /([^\s=/>"']+)(?:\s*=\s*("[^"]*"|'[^']*'|[^\s>"']+))?/g

function attributesOf(raw) {
  const attrs = []
  for (const m of raw.matchAll(ATTR)) {
    let value = m[2] ?? ''
    if (value.length >= 2 && (value[0] === '"' || value[0] === "'")) value = value.slice(1, -1)
    attrs.push({ name: m[1].toLowerCase(), value })
  }
  return attrs
}

const isOwnPath = (url) => url.startsWith('/') && !url.startsWith('//')

/**
 * What in this HTML the CSP refuses, as readable lines ([] when clean). `stats`, when given, is filled with what was looked at, so a caller can refuse an
 * empty subject.
 */
export function findViolations(html, stats = { tags: 0, scripts: 0 }) {
  const out = []
  const source = html.replace(/<!--[\s\S]*?-->/g, '')
  stats.tags = 0
  stats.scripts = 0
  TAG.lastIndex = 0
  let m
  while ((m = TAG.exec(source)) !== null) {
    const closing = m[1] === '/'
    const name = m[2].toLowerCase()
    if (closing) continue
    stats.tags++
    const attrs = attributesOf(m[3])

    for (const a of attrs) {
      if (/^on[a-z]/.test(a.name)) out.push(`an inline event handler: <${name} ${a.name}=...>`)
      if ((a.name === 'href' || a.name === 'src' || a.name === 'action') && /^\s*javascript:/i.test(a.value)) out.push(`a javascript: URL in <${name} ${a.name}>`)
    }

    if (name === 'script') {
      stats.scripts++
      const src = attrs.find((a) => a.name === 'src')
      const end = source.slice(TAG.lastIndex).search(/<\/script/i)
      const body = end < 0 ? source.slice(TAG.lastIndex) : source.slice(TAG.lastIndex, TAG.lastIndex + end)
      if (body.trim() !== '') out.push('an inline script: a <script> with a body')
      if (src === undefined) out.push('a <script> without a src')
      else if (!isOwnPath(src.value)) out.push(`a script from another origin: ${src.value}`)
    }

    if (name === 'link') {
      const rel = (attrs.find((a) => a.name === 'rel')?.value ?? '').toLowerCase()
      const href = attrs.find((a) => a.name === 'href')?.value ?? ''
      if ((rel.includes('stylesheet') || rel.includes('modulepreload')) && !isOwnPath(href)) out.push(`a ${rel} from another origin: ${href}`)
    }
  }
  return out
}

// ---- the instrument's own cases: each reject case asserts the REASON it fails for ----

const OK_PAGE = '<!doctype html><html><head><meta charset="UTF-8"><link rel="icon" href="data:,"><script type="module" crossorigin src="/assets/index-abc.js"></script><link rel="stylesheet" crossorigin href="/assets/index-abc.css"></head><body><div id="root"></div></body></html>'

export const SELFTEST_CASES = [
  { name: 'the real shape of a built page is clean', html: OK_PAGE, want: null },
  { name: 'an inline script body', html: OK_PAGE.replace('</head>', '<script>window.__x=1</script></head>'), want: 'an inline script' },
  { name: 'an inline module script', html: OK_PAGE.replace('</head>', '<script type="module">import("/a.js")</script></head>'), want: 'an inline script' },
  { name: 'a script with no src', html: OK_PAGE.replace('</head>', '<script type="module"></script></head>'), want: 'without a src' },
  { name: 'an onclick handler', html: OK_PAGE.replace('<div id="root">', '<div id="root" onclick="go()">'), want: 'an inline event handler' },
  { name: 'an onload handler in single quotes', html: OK_PAGE.replace('<body>', "<body onload='init()'>"), want: 'an inline event handler' },
  { name: 'a javascript: link', html: OK_PAGE.replace('<div id="root">', '<a href="javascript:alert(1)">x</a><div id="root">'), want: 'a javascript: URL' },
  { name: 'a script from another origin', html: OK_PAGE.replace('/assets/index-abc.js', 'https://cdn.example.test/x.js'), want: 'another origin' },
  { name: 'a protocol-relative script', html: OK_PAGE.replace('/assets/index-abc.js', '//cdn.example.test/x.js'), want: 'another origin' },
  { name: 'a stylesheet from another origin', html: OK_PAGE.replace('/assets/index-abc.css', 'https://fonts.example.test/f.css'), want: 'another origin' },
  { name: 'a > inside a quoted attribute does not hide an inline script', html: OK_PAGE.replace('</head>', '<meta content="a>b"><script>1</script></head>'), want: 'an inline script' },
  { name: 'a commented-out script is not a script', html: OK_PAGE.replace('</head>', '<!-- <script>1</script> --></head>'), want: null },
  { name: 'the word "onclick" in text is not a handler', html: OK_PAGE.replace('<div id="root">', '<p>onclick=1</p><div id="root">'), want: null },
]

export const SELFTEST_FLOOR = 10

export function selftest(log = console) {
  let failed = 0
  for (const c of SELFTEST_CASES) {
    const found = findViolations(c.html)
    const ok = c.want === null ? found.length === 0 : found.length > 0 && found.some((l) => l.includes(c.want))
    if (!ok) {
      failed++
      log.error(`check-csp selftest: FAIL - ${c.name}: wanted ${c.want === null ? 'clean' : `a violation naming "${c.want}"`}, got ${JSON.stringify(found)}`)
    }
  }
  if (SELFTEST_CASES.length < SELFTEST_FLOOR) {
    failed++
    log.error(`check-csp selftest: FAIL - only ${SELFTEST_CASES.length} cases, the floor is ${SELFTEST_FLOOR}`)
  }
  if (failed === 0) log.log(`check-csp selftest: OK - ${SELFTEST_CASES.length} cases, each reject case named the reason it fails for`)
  return failed === 0 ? 0 : 1
}

// ---- the check ----

export function check(distDir, log = console) {
  let html
  try {
    html = readFileSync(join(distDir, 'index.html'), 'utf8')
  } catch {
    log.error(`check-csp: cannot read ${join(distDir, 'index.html')} (run "npm run build" first)`)
    return 2
  }
  const stats = { tags: 0, scripts: 0 }
  const violations = findViolations(html, stats)
  if (stats.scripts === 0) {
    log.error('check-csp: the built page holds no <script> at all: the instrument matched nothing, so it cannot call the page clean')
    return 2
  }
  if (violations.length > 0) {
    log.error(`check-csp: FAIL - ${violations.length} thing(s) the Content-Security-Policy refuses in ${join(distDir, 'index.html')}:`)
    for (const v of violations) log.error(`  - ${v}`)
    return 1
  }
  log.log(`check-csp: OK - ${stats.tags} tags and ${stats.scripts} script tag(s) read in ${join(distDir, 'index.html')}: no inline script, no event handler attribute, no javascript: URL, nothing from another origin`)
  return 0
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const args = process.argv.slice(2)
  if (args.includes('--selftest')) process.exit(selftest())
  const dir = args.find((a) => !a.startsWith('--')) ?? join(import.meta.dirname, '..', 'dist')
  process.exit(check(dir))
}

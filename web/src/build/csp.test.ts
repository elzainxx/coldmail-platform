// The page the build makes must hold nothing the platform's Content-Security-Policy refuses (scripts/check-csp.mjs, and the guard in vite.config.ts that
// fails the build itself). The instrument is tried on pages that must be refused, each for its own reason.
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { describe, expect, it, vi } from 'vitest'
import { SELFTEST_CASES, SELFTEST_FLOOR, check, findViolations, selftest } from '../../scripts/check-csp.mjs'

const quiet = () => ({ log: vi.fn(), error: vi.fn() })

describe('findViolations', () => {
  it('has a case for every kind of thing it refuses, and each reject case names the reason it fails for', () => {
    expect(SELFTEST_CASES.length).toBeGreaterThanOrEqual(SELFTEST_FLOOR)
    const rejects = SELFTEST_CASES.filter((c) => c.want !== null)
    expect(rejects.length).toBeGreaterThanOrEqual(8)
    for (const c of rejects) {
      const found = findViolations(c.html)
      expect(found.length, c.name).toBeGreaterThan(0)
      expect(found.some((l) => l.includes(c.want as string)), `${c.name}: ${JSON.stringify(found)}`).toBe(true)
    }
    for (const c of SELFTEST_CASES.filter((x) => x.want === null)) expect(findViolations(c.html), c.name).toEqual([])
  })

  it('refuses an inline script and an inline handler, but not the module script of the app', () => {
    const app = '<html><head><script type="module" src="/assets/app.js"></script></head><body><div id="root"></div></body></html>'
    expect(findViolations(app)).toEqual([])
    expect(findViolations(app.replace('</body>', '<script>alert(1)</script></body>'))).toContain('an inline script: a <script> with a body')
    expect(findViolations(app.replace('<div', '<div onclick="x()"'))).toEqual(['an inline event handler: <div onclick=...>'])
  })

  it('counts what it looked at, so an empty subject can be told from a clean one', () => {
    const stats = { tags: 0, scripts: 0 }
    findViolations('<html><body><p>hello</p></body></html>', stats)
    expect(stats.scripts).toBe(0)
    expect(stats.tags).toBeGreaterThan(2)
  })
})

describe('the instrument itself', () => {
  it('its selftest passes, and says how many cases it ran', () => {
    const log = quiet()
    expect(selftest(log)).toBe(0)
    expect(log.log).toHaveBeenCalledWith(expect.stringMatching(/OK - \d+ cases/))
  })
})

describe('check on a directory', () => {
  function distWith(html: string | null): string {
    const dir = mkdtempSync(join(tmpdir(), 'csp-'))
    if (html !== null) writeFileSync(join(dir, 'index.html'), html)
    return dir
  }
  const GOOD = '<!doctype html><html><head><script type="module" crossorigin src="/assets/index-1.js"></script><link rel="stylesheet" href="/assets/index-1.css"></head><body><div id="root"></div></body></html>'

  it('is clean (0) for a page of the shape the build makes', () => {
    const dir = distWith(GOOD)
    try {
      const log = quiet()
      expect(check(dir, log)).toBe(0)
      expect(log.log).toHaveBeenCalledWith(expect.stringContaining('no inline script'))
    } finally {
      rmSync(dir, { recursive: true, force: true })
    }
  })

  it('fails (1) for an inline script, and lists it', () => {
    const dir = distWith(GOOD.replace('</body>', '<script>window.x=1</script></body>'))
    try {
      const log = quiet()
      expect(check(dir, log)).toBe(1)
      expect(log.error).toHaveBeenCalledWith(expect.stringContaining('an inline script'))
    } finally {
      rmSync(dir, { recursive: true, force: true })
    }
  })

  it('fails (1) for an on* attribute', () => {
    const dir = distWith(GOOD.replace('<div id="root">', '<div id="root" onmouseover="x()">'))
    try {
      expect(check(dir, quiet())).toBe(1)
    } finally {
      rmSync(dir, { recursive: true, force: true })
    }
  })

  it('cannot run (2), which is not a pass, when there is no built page or when the page holds no script at all', () => {
    const empty = distWith(null)
    const noScript = distWith('<html><body><p>nothing</p></body></html>')
    try {
      const a = quiet()
      expect(check(empty, a)).toBe(2)
      expect(a.error).toHaveBeenCalledWith(expect.stringContaining('npm run build'))
      const b = quiet()
      expect(check(noScript, b)).toBe(2)
      expect(b.error).toHaveBeenCalledWith(expect.stringContaining('matched nothing'))
    } finally {
      rmSync(empty, { recursive: true, force: true })
      rmSync(noScript, { recursive: true, force: true })
    }
  })
})

describe('the sources of the page', () => {
  it('index.html has no inline script and no inline handler', () => {
    const html = readFileSync('index.html', 'utf8')
    const stats = { tags: 0, scripts: 0 }
    expect(findViolations(html, stats)).toEqual([])
    expect(stats.scripts).toBeGreaterThanOrEqual(1) // it did look at the page's script
  })

  it('vite.config.ts switches the module preload polyfill off and runs the guard on the built page', () => {
    const config = readFileSync('vite.config.ts', 'utf8')
    expect(config).toMatch(/modulePreload:\s*\{\s*polyfill:\s*false\s*\}/)
    expect(config).toContain("from './scripts/check-csp.mjs'")
    expect(config).toContain('findViolations(')
  })

  it('paired control: the same check on a page WITH an inline script fails', () => {
    const html = readFileSync('index.html', 'utf8').replace('</body>', '<script>boot()</script></body>')
    expect(findViolations(html).length).toBeGreaterThan(0)
  })
})

// Tokens live in memory ONLY. Three independent proofs: (1) every browser storage API is watched while a session goes through its whole life;
// (2) a scan of every source file for a storage API (one file is allowed, the theme); (3) the page that comes back from the back-forward cache is wiped.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { installPageshowGuard } from './lifecycle.ts'
import { createSession } from './session.ts'
import { fakeGoTrue, fakeJwt } from '../test/helpers.tsx'

/** Spies on every way a page can keep something: Web Storage (local and session), cookies, IndexedDB. */
function watchStorage() {
  const seen: string[] = []
  const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation((k: string, v: string) => {
    seen.push(`setItem(${k}, ${v})`)
  })
  const cookie = vi.spyOn(document, 'cookie', 'set').mockImplementation((v: string) => {
    seen.push(`cookie=${v}`)
  })
  const open = vi.fn((name: string) => {
    seen.push(`indexedDB.open(${name})`)
  })
  vi.stubGlobal('indexedDB', { open, deleteDatabase: open })
  return {
    seen,
    restore() {
      setItem.mockRestore()
      cookie.mockRestore()
      vi.unstubAllGlobals()
    },
  }
}

const SECRET_ACCESS = fakeJwt(Math.floor(Date.now() / 1000) + 3600, { marker: 'SECRET-ACCESS-MARKER' })
const SECRET_REFRESH = 'SECRET-REFRESH-MARKER'

let watch: ReturnType<typeof watchStorage>
beforeEach(() => {
  watch = watchStorage()
})
afterEach(() => {
  watch.restore()
})

describe('no storage', () => {
  it('a session goes through its whole life (sign in, refresh, invite, set password, sign out) and nothing is written anywhere', async () => {
    const gotrue = fakeGoTrue({
      signInWithPassword: vi.fn(async () => ({ accessToken: SECRET_ACCESS, refreshToken: SECRET_REFRESH, expiresIn: 3600 })),
      refresh: vi.fn(async () => ({ accessToken: SECRET_ACCESS, refreshToken: `${SECRET_REFRESH}-2`, expiresIn: 3600 })),
    })
    const session = createSession()
    session.configure(gotrue)
    await session.signIn('a@b.test', 'pw')
    await session.refresh()
    await session.signOut()
    session.adopt({ accessToken: SECRET_ACCESS, refreshToken: SECRET_REFRESH, expiresIn: 3600 }, 'invite')
    await session.setPassword('a brand new long password')
    session.clear()

    expect(watch.seen).toEqual([])
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
    expect(JSON.stringify(localStorage) + JSON.stringify(sessionStorage)).not.toContain('SECRET')
  })

  it('paired control: the same watch DOES see a token written by a store that leaks, in each of the places', () => {
    const token = SECRET_ACCESS
    localStorage.setItem('token', token)
    sessionStorage.setItem('token', token)
    document.cookie = `token=${token}`
    ;(globalThis as unknown as { indexedDB: { open: (n: string) => void } }).indexedDB.open(`db-${token}`)
    expect(watch.seen).toHaveLength(4)
    expect(watch.seen.every((line) => line.includes(token))).toBe(true)
  })
})

// A scan over the source: the storage APIs are named in exactly one non-test file.
const sources = import.meta.glob(['/src/**/*.{ts,tsx}', '!/src/**/*.test.{ts,tsx}', '!/src/**/*.d.ts'], { query: '?raw', import: 'default', eager: true }) as Record<string, string>
const STORAGE_API = /\b(localStorage|sessionStorage|indexedDB|openDatabase|document\.cookie|cookieStore|caches\.(open|match)|navigator\.storage)\b/

function usesStorage(source: string): boolean {
  const code = source.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1')
  return STORAGE_API.test(code)
}

describe('no storage API in the source', () => {
  it('only the theme file names one', () => {
    const files = Object.keys(sources)
    expect(files.length).toBeGreaterThanOrEqual(25) // the scan found the app
    const offenders = Object.entries(sources)
      .filter(([, text]) => usesStorage(text))
      .map(([path]) => path)
    expect(offenders).toEqual(['/src/shell/theme.ts'])
  })

  it('paired control: the scan does see a storage call, and ignores one in a comment', () => {
    expect(usesStorage("window.localStorage.setItem('a', b)")).toBe(true)
    expect(usesStorage('document.cookie = x')).toBe(true)
    expect(usesStorage('const db = indexedDB.open("x")')).toBe(true)
    expect(usesStorage("// never localStorage\nconst a = 1 /* indexedDB */")).toBe(false)
  })

  it('the one file stores a single word, "light" or "dark", under one key, and nothing about a user', () => {
    const theme = sources['/src/shell/theme.ts'] ?? ''
    expect(theme.match(/setItem\(/g)).toHaveLength(1)
    expect(theme).toContain("setItem(KEY, theme)")
    expect(theme).not.toMatch(/access_token|refresh_token|getAccessToken|auth\/session/)
  })
})

describe('the back-forward cache', () => {
  function pageshow(persisted: boolean): Event {
    const e = new Event('pageshow')
    Object.defineProperty(e, 'persisted', { value: persisted })
    return e
  }

  it('a page restored from the cache is wiped and sent to the sign-in page', () => {
    const clear = vi.fn()
    window.history.replaceState(null, '', '/jobs/1')
    const stop = installPageshowGuard({ clear })
    window.dispatchEvent(pageshow(true))
    expect(clear).toHaveBeenCalledTimes(1)
    expect(window.location.pathname).toBe('/sign-in')
    stop()
  })

  it('paired control: an ordinary page show does nothing', () => {
    const clear = vi.fn()
    window.history.replaceState(null, '', '/jobs/1')
    const stop = installPageshowGuard({ clear })
    window.dispatchEvent(pageshow(false))
    expect(clear).not.toHaveBeenCalled()
    expect(window.location.pathname).toBe('/jobs/1')
    stop()
  })

  it('stops listening when it is removed', () => {
    const clear = vi.fn()
    installPageshowGuard({ clear })()
    window.dispatchEvent(pageshow(true))
    expect(clear).not.toHaveBeenCalled()
  })

  it('with a real session: the tokens are gone after the restore', async () => {
    const session = createSession()
    session.configure(fakeGoTrue())
    await session.signIn('a@b.test', 'pw')
    const stop = installPageshowGuard(session)
    window.dispatchEvent(pageshow(true))
    expect(session.getAccessToken()).toBeNull()
    expect(session.getSnapshot().status).toBe('signed-out')
    stop()
  })
})

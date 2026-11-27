// WCAG 2 contrast, computed from the values in tokens.css. Used by tokens.test.ts only: nothing in the app imports this
// file, so it is not in the bundle.

import { STATUS_TOKENS } from '../ui/status.ts'

export type TokenMap = Record<string, string>

/** Red, green and blue (0 to 255) of a #rgb or #rrggbb colour. Anything else throws: a token that is not a hex colour cannot be checked. */
export function parseHex(hex: string): [number, number, number] {
  const m = /^#([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(hex.trim())
  if (!m) throw new Error(`not a hex colour: "${hex}"`)
  let h = m[1] ?? ''
  if (h.length === 3) h = [...h].map((c) => c + c).join('')
  const n = parseInt(h, 16)
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255]
}

function channel(c: number): number {
  const s = c / 255
  return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4
}

export function relativeLuminance(hex: string): number {
  const [r, g, b] = parseHex(hex)
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b)
}

/** The WCAG contrast ratio, 1 to 21. */
export function contrastRatio(a: string, b: string): number {
  const x = relativeLuminance(a)
  const y = relativeLuminance(b)
  return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05)
}

// ---- reading tokens.css ----

/** The text between the braces that follow `header` (balanced), or null when the header is not in the css. */
export function blockBody(css: string, header: string): string | null {
  const at = css.indexOf(header)
  if (at < 0) return null
  const open = css.indexOf('{', at)
  if (open < 0) return null
  let depth = 0
  for (let i = open; i < css.length; i++) {
    const ch = css[i]
    if (ch === '{') depth++
    else if (ch === '}') {
      depth--
      if (depth === 0) return css.slice(open + 1, i)
    }
  }
  return null
}

/** The custom properties declared directly in a block body (`--name: value;`), comments removed. */
export function declarations(body: string): TokenMap {
  const out: TokenMap = {}
  const clean = body.replace(/\/\*[\s\S]*?\*\//g, '')
  for (const m of clean.matchAll(/(--[a-z0-9-]+)\s*:\s*([^;{}]+);/gi)) {
    out[m[1] as string] = (m[2] as string).trim()
  }
  return out
}

export interface ThemeBlocks {
  light: TokenMap
  /** dark values under the system preference (not when the user chose light) */
  darkMedia: TokenMap
  /** dark values under [data-theme="dark"] */
  darkAttr: TokenMap
}

export function readThemeBlocks(css: string): ThemeBlocks {
  const light = blockBody(css, '\n:root {')
  const media = blockBody(css, '@media (prefers-color-scheme: dark)')
  const darkMedia = media === null ? null : blockBody(media, ':root:not([data-theme="light"])')
  const darkAttr = blockBody(css, ':root[data-theme="dark"]')
  if (light === null || darkMedia === null || darkAttr === null) {
    throw new Error('tokens.css: a theme block is missing (:root, the dark media block or :root[data-theme="dark"])')
  }
  return { light: declarations(light), darkMedia: declarations(darkMedia), darkAttr: declarations(darkAttr) }
}

// ---- what must hold ----

export interface Pair {
  /** the foreground token (text, or the line or ring for a non-text pair) */
  fg: string
  /** the background it sits on */
  bg: string
  /** 4.5 for text, 3 for a boundary or a ring that must be seen */
  min: number
  what: string
}

const SURFACES = ['--c-surface', '--c-surface-raised', '--c-surface-sunken', '--c-surface-hover'] as const

export function requiredPairs(): Pair[] {
  const pairs: Pair[] = []
  for (const s of SURFACES) {
    pairs.push({ fg: '--c-text', bg: s, min: 4.5, what: `body text on ${s}` })
    pairs.push({ fg: '--c-text-muted', bg: s, min: 4.5, what: `muted text on ${s}` })
    pairs.push({ fg: '--c-accent', bg: s, min: 4.5, what: `link text on ${s}` })
    pairs.push({ fg: '--c-danger', bg: s, min: 4.5, what: `error text on ${s}` })
    pairs.push({ fg: '--c-border-strong', bg: s, min: 3, what: `input and control boundary on ${s}` })
    pairs.push({ fg: '--c-focus', bg: s, min: 3, what: `focus ring on ${s}` })
  }
  pairs.push({ fg: '--c-accent-text', bg: '--c-accent', min: 4.5, what: 'text on an accent button' })
  pairs.push({ fg: '--c-accent-text', bg: '--c-accent-hover', min: 4.5, what: 'text on a hovered accent button' })
  pairs.push({ fg: '--c-danger-text', bg: '--c-danger', min: 4.5, what: 'text on a danger button' })
  pairs.push({ fg: '--c-danger-text', bg: '--c-danger-hover', min: 4.5, what: 'text on a hovered danger button' })
  for (const s of STATUS_TOKENS) {
    pairs.push({ fg: `--c-status-${s}-fg`, bg: `--c-status-${s}-bg`, min: 4.5, what: `the ${s} badge text` })
    for (const surface of ['--c-surface', '--c-surface-raised']) {
      pairs.push({ fg: `--c-status-${s}-fg`, bg: surface, min: 3, what: `the ${s} colour as a mark on ${surface}` })
    }
  }
  return pairs
}

/** The pairs that fail for a theme, as readable lines. A token that is missing or not a hex colour is a failure too, never a skip. */
export function failingPairs(theme: TokenMap, pairs: Pair[]): string[] {
  const failures: string[] = []
  for (const p of pairs) {
    const fg = theme[p.fg]
    const bg = theme[p.bg]
    if (fg === undefined || bg === undefined) {
      failures.push(`${p.what}: ${fg === undefined ? p.fg : p.bg} is not defined`)
      continue
    }
    try {
      const ratio = contrastRatio(fg, bg)
      if (ratio < p.min) failures.push(`${p.what}: ${fg} on ${bg} is ${ratio.toFixed(2)}:1, needs ${p.min}:1`)
    } catch (e) {
      failures.push(`${p.what}: ${(e as Error).message}`)
    }
  }
  return failures
}

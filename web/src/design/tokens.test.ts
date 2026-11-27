import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import {
  blockBody,
  contrastRatio,
  declarations,
  failingPairs,
  readThemeBlocks,
  relativeLuminance,
  requiredPairs,
  type TokenMap,
} from './contrast.ts'
import { STATUS_TOKENS } from '../ui/status.ts'

// vitest's css pipeline turns a .css import into an empty string, even with ?raw: read the file itself.
// (vitest runs from web/, as the web workflow does.)
const css = readFileSync('src/design/tokens.css', 'utf8')
const blocks = readThemeBlocks(css)
const pairs = requiredPairs()
const colourNames = Object.keys(blocks.light).filter((k) => k.startsWith('--c-') && !k.startsWith('--c-shadow-'))

describe('contrast arithmetic', () => {
  it('matches the published WCAG values', () => {
    expect(relativeLuminance('#ffffff')).toBeCloseTo(1, 5)
    expect(relativeLuminance('#000000')).toBeCloseTo(0, 5)
    expect(contrastRatio('#000000', '#ffffff')).toBeCloseTo(21, 1)
    expect(contrastRatio('#777777', '#ffffff')).toBeCloseTo(4.48, 1) // the classic grey that just misses AA
    expect(contrastRatio('#767676', '#ffffff')).toBeGreaterThanOrEqual(4.5)
  })

  it('reads #rgb and refuses a value that is not a hex colour', () => {
    expect(contrastRatio('#fff', '#000')).toBeCloseTo(21, 1)
    expect(() => contrastRatio('rgb(0 0 0)', '#fff')).toThrow(/not a hex colour/)
  })
})

describe('tokens.css', () => {
  it('found its three theme blocks and a real number of colours (a parser that finds nothing must be red)', () => {
    expect(colourNames.length).toBeGreaterThanOrEqual(30)
    expect(pairs.length).toBeGreaterThanOrEqual(45)
  })

  it('has a dark value for every light colour, and the two dark blocks are identical', () => {
    const missing = Object.keys(blocks.light).filter((k) => blocks.darkAttr[k] === undefined)
    expect(missing).toEqual([])
    expect(Object.keys(blocks.darkAttr).sort()).toEqual(Object.keys(blocks.light).sort())
    expect(blocks.darkMedia).toEqual(blocks.darkAttr)
  })

  it('paired control: the dark-blocks comparison does see a value that drifts', () => {
    const drifted: TokenMap = { ...blocks.darkMedia, '--c-surface': '#000001' }
    expect(drifted).not.toEqual(blocks.darkAttr)
  })

  it('uses hex colours only (so contrast can be computed from this file)', () => {
    for (const theme of [blocks.light, blocks.darkAttr]) {
      for (const name of colourNames) {
        expect(theme[name], name).toMatch(/^#[0-9a-f]{6}$/i)
      }
    }
  })

  it('every status token has a foreground and a background colour in both themes', () => {
    for (const s of STATUS_TOKENS) {
      for (const theme of [blocks.light, blocks.darkAttr]) {
        expect(theme[`--c-status-${s}-fg`], `${s} fg`).toBeDefined()
        expect(theme[`--c-status-${s}-bg`], `${s} bg`).toBeDefined()
      }
    }
  })

  it('every status colour has a Tailwind theme entry', () => {
    for (const s of STATUS_TOKENS) {
      expect(css).toContain(`--color-status-${s}: var(--c-status-${s}-fg);`)
      expect(css).toContain(`--color-status-${s}-bg: var(--c-status-${s}-bg);`)
    }
  })

  it('is 14px at the base and uses system fonts only (no web font: the CSP allows none)', () => {
    const theme = blockBody(css, '@theme inline') ?? ''
    expect(declarations(theme)['--text-base']).toBe('0.875rem')
    expect(css).not.toMatch(/@font-face|fonts\.googleapis|url\(/i)
  })
})

describe('contrast, light theme', () => {
  it('meets WCAG AA for every text pair (4.5:1) and every boundary or ring (3:1)', () => {
    expect(failingPairs(blocks.light, pairs)).toEqual([])
  })
})

describe('contrast, dark theme', () => {
  it('meets WCAG AA for every text pair (4.5:1) and every boundary or ring (3:1)', () => {
    expect(failingPairs(blocks.darkAttr, pairs)).toEqual([])
  })
})

describe('the contrast check can fail', () => {
  it('names the pair when a text colour drops under 4.5:1', () => {
    const tampered: TokenMap = { ...blocks.light, '--c-text-muted': '#9aa1ad' }
    const failures = failingPairs(tampered, pairs)
    expect(failures.length).toBeGreaterThan(0)
    expect(failures.some((f) => f.includes('muted text on --c-surface:'))).toBe(true)
  })

  it('names a status badge whose text is too faint, in the dark theme too', () => {
    const tampered: TokenMap = { ...blocks.darkAttr, '--c-status-risky-fg': '#5a4a22' }
    expect(failingPairs(tampered, pairs).some((f) => f.includes('the risky badge text'))).toBe(true)
  })

  it('is red for a token that is missing or not a hex colour, never a skip', () => {
    const missing: TokenMap = { ...blocks.light }
    delete missing['--c-focus']
    expect(failingPairs(missing, pairs).some((f) => f.includes('--c-focus is not defined'))).toBe(true)
    const named: TokenMap = { ...blocks.light, '--c-accent': 'rebeccapurple' }
    expect(failingPairs(named, pairs).some((f) => f.includes('not a hex colour'))).toBe(true)
  })
})

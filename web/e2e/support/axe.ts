// The accessibility check of a screen: axe-core, WCAG 2 level A and AA rules (colour contrast is one of the AA rules), run on the page as it is NOW, failing on ANY violation. Chunk A11 asks for it on every main
// screen: `await expectNoA11yViolations(page)` after the screen has settled (the data is in, the spinner is gone).
//
// Two things keep it from being a check of nothing (a gate whose subject is empty is red):
//   - axe must have EVALUATED rules (a page that was not ready, a frame it could not enter, gives an empty result that "has no violations");
//   - the colour-contrast rule must have been evaluated, as a pass, a violation or an "incomplete" (text over an image, which a person must judge): a page with no text at all is not a screen.
// `include` narrows the check to one region (a CSS selector) and `exclude` leaves regions out (the sandboxed preview of a user's own HTML is their content, not the app's: exclude its frame, never the page).
// Violations are reported as rule, impact, how many nodes and a short selector of each: never the text of the page (it may hold an address of the test users).
import { AxeBuilder } from '@axe-core/playwright'
import type { Page } from '@playwright/test'

export interface AxeOptions {
  /** A CSS selector: only this region is analysed. */
  include?: string
  /** CSS selectors of regions to leave out. */
  exclude?: string[]
}

/** The rule sets the project holds its screens to. */
export const AXE_TAGS = ['wcag2a', 'wcag2aa'] as const

export async function expectNoA11yViolations(page: Page, options: AxeOptions = {}): Promise<void> {
  let builder = new AxeBuilder({ page }).withTags([...AXE_TAGS])
  if (options.include !== undefined) builder = builder.include(options.include)
  for (const selector of options.exclude ?? []) builder = builder.exclude(selector)
  const result = await builder.analyze()

  if (result.passes.length + result.violations.length === 0) {
    throw new Error('axe evaluated no rule on this page: the check proved nothing (the page was not ready, or axe could not enter it)')
  }
  const evaluated = new Set([...result.passes, ...result.violations, ...result.incomplete].map((r) => r.id))
  if (!evaluated.has('color-contrast')) {
    throw new Error('axe did not evaluate the colour-contrast rule on this page: it has no text to measure, so the contrast half of the check proved nothing')
  }
  if (result.violations.length > 0) {
    const lines = result.violations.map((v) => {
      const where = v.nodes.slice(0, 3).map((n) => n.target.join(' ')).join(' | ')
      return `  - ${v.id} (${v.impact ?? 'unrated'}), ${v.nodes.length} node(s): ${v.help}. First: ${where}`
    })
    throw new Error(`${result.violations.length} accessibility violation(s) (WCAG 2 A and AA, axe-core):\n${lines.join('\n')}`)
  }
}

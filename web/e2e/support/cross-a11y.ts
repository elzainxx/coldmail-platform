// The accessibility checks that axe does not make for a spec of chunk A11: that every input, select and textarea a person can see has an accessible name (counted by ROLE against what is on the page, and for
// the controls that have no role, by the DOM's own label), and that the keyboard reaches controls in order with a visible focus ring (the computed style of `:focus-visible`).
//
// Neither check prints a value of the page: a control is named by its tag and type, a focus stop by its tag and the first words of its accessible name (a label of the app's own copy).
import { expect, type Page } from '@playwright/test'

interface ControlReport {
  tag: string
  type: string
  named: boolean
}

interface VisibleCounts {
  checkbox: number
  radio: number
  combobox: number
  textbox: number
  searchbox: number
}

/**
 * Every visible `input`, `select` and `textarea` of the document (the page itself: the frame of a user's own HTML is not read) has an accessible name from a label, `aria-label` or `aria-labelledby`
 * (a placeholder or a title is not a name). The same controls are then counted by role: the number of checkboxes, radios, selects, text boxes and search boxes a person can see equals the number that
 * `getByRole` finds WITH a non-empty accessible name. Password, file and date inputs have no role of their own, so the DOM check above is what covers them.
 */
export async function expectEveryControlNamed(page: Page): Promise<void> {
  const seen = await page.evaluate(() => {
    const visible = (el: Element): boolean => el.checkVisibility({ checkVisibilityCSS: true }) && el.getClientRects().length > 0
    const textOf = (id: string): string => document.getElementById(id)?.textContent?.trim() ?? ''
    const nameOf = (el: HTMLElement): string => {
      const by = el.getAttribute('aria-labelledby')
      if (by !== null && by.trim() !== '') {
        const joined = by.split(/\s+/).map(textOf).join(' ').trim()
        if (joined !== '') return joined
      }
      const own = el.getAttribute('aria-label')?.trim() ?? ''
      if (own !== '') return own
      const labels = (el as HTMLInputElement).labels
      return labels === null || labels === undefined ? '' : Array.from(labels, (l) => l.textContent?.trim() ?? '').join(' ').trim()
    }
    const controls = Array.from(document.querySelectorAll<HTMLElement>('input:not([type=hidden]), select, textarea')).filter(visible)
    const report: ControlReport[] = controls.map((el) => ({ tag: el.tagName.toLowerCase(), type: el.getAttribute('type') ?? '', named: nameOf(el) !== '' }))
    const textLike = (el: HTMLElement): boolean => {
      if (el.tagName === 'TEXTAREA') return true
      if (el.tagName !== 'INPUT') return false
      const type = (el.getAttribute('type') ?? 'text').toLowerCase()
      // Playwright gives a password input the role textbox too
      return ['text', 'email', 'tel', 'url', 'password'].includes(type) && !el.hasAttribute('list')
    }
    const counts: VisibleCounts = {
      checkbox: controls.filter((el) => el.matches('input[type=checkbox]')).length,
      radio: controls.filter((el) => el.matches('input[type=radio]')).length,
      combobox: controls.filter((el) => el.matches('select:not([multiple])')).length,
      textbox: controls.filter(textLike).length,
      searchbox: controls.filter((el) => el.matches('input[type=search]')).length,
    }
    return { report, counts }
  })

  const unnamed = seen.report.filter((c) => !c.named)
  expect(
    unnamed.length,
    `${unnamed.length} visible control(s) have no accessible name (a label, aria-label or aria-labelledby): ${unnamed.map((c) => `${c.tag}${c.type === '' ? '' : `[type=${c.type}]`}`).join(', ')}`,
  ).toBe(0)

  for (const role of ['checkbox', 'radio', 'combobox', 'textbox', 'searchbox'] as const) {
    const named = await page.getByRole(role, { name: /\S/ }).count()
    expect(named, `the page shows ${seen.counts[role]} ${role} control(s) and ${named} of them have an accessible name`).toBe(seen.counts[role])
  }
}

export interface FocusStop {
  tag: string
  /** the first words of the control's accessible name (aria-label, or its text): the app's own words */
  name: string
  /** `:focus-visible` matches the element */
  focusVisible: boolean
  outlineStyle: string
  outlineWidthPx: number
}

/** The element that has the keyboard focus now, with the computed style of its focus ring. */
export async function focusStop(page: Page): Promise<FocusStop> {
  return await page.evaluate(() => {
    const el = document.activeElement
    if (el === null || el === document.body) return { tag: 'body', name: '', focusVisible: false, outlineStyle: 'none', outlineWidthPx: 0 }
    const cs = getComputedStyle(el)
    const label = el.getAttribute('aria-label') ?? el.textContent ?? ''
    return {
      tag: el.tagName.toLowerCase(),
      name: label.trim().slice(0, 40),
      focusVisible: el.matches(':focus-visible'),
      outlineStyle: cs.outlineStyle,
      outlineWidthPx: Number.parseFloat(cs.outlineWidth) || 0,
    }
  })
}

/** A focus stop that a keyboard user can SEE: `:focus-visible` matches and the outline is drawn, 2 px or wider (the app's one ring is `2px solid`, index.css). */
export function hasVisibleRing(stop: FocusStop): boolean {
  return stop.focusVisible && stop.outlineStyle !== 'none' && stop.outlineStyle !== 'hidden' && stop.outlineWidthPx >= 2
}

/** Presses Tab `count` times and returns each stop, in order. */
export async function tabStops(page: Page, count: number): Promise<FocusStop[]> {
  const stops: FocusStop[] = []
  for (let i = 0; i < count; i++) {
    await page.keyboard.press('Tab')
    stops.push(await focusStop(page))
  }
  return stops
}

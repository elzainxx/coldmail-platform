// The screenshots of the main screens: `await shot(page, 'dashboard')` writes `<E2E_SHOTS_DIR>/dashboard.png`, a full-page PNG, and returns its path. The run's directory is uploaded by the harness workflow
// as the `e2e-artifacts` artifact, so a person reviews every main screen of every run without reproducing it. The stage asserts that at least E2E_MIN_SHOTS pictures exist (tests/harness/e2e/stage.sh).
//
// One name per screen and state, lower case letters, digits and hyphens (`jobs-table`, `job-detail-running`, `settings-vendor-opt-out`): the name is the file's name, a second picture under the same name
// replaces the first. Animations are frozen and the caret hidden so that two runs of one screen give the same picture. A picture shows what a person would see: do not take one while a password is typed in
// clear text, and remember that an artifact of a public repository is public.
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'
import type { Page } from '@playwright/test'
import { shotsDir } from './env.ts'

const NAME = /^[a-z0-9][a-z0-9-]*$/

export async function shot(page: Page, name: string): Promise<string> {
  if (!NAME.test(name)) {
    throw new Error(`the screenshot name '${name}' must be lower case letters, digits and hyphens, starting with a letter or a digit`)
  }
  const dir = shotsDir()
  mkdirSync(dir, { recursive: true })
  const path = join(dir, `${name}.png`)
  await page.screenshot({ path, fullPage: true, animations: 'disabled', caret: 'hide' })
  return path
}

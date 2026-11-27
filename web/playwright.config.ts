// The browser end-to-end tests (Phase 6, chunk A9; Docs/phase-6-spec.md decision 9). They run ONLY in the harness's `e2e` scope (tests/harness/e2e/stage.sh), inside Playwright's own image on the
// harness's private network, against the real stack: the platform serving this app and the API from one origin, a real GoTrue, modules 1 to 3. There is no other way to run them (the stack needs Docker,
// which a developer's machine may not have: the GitHub runner is the evidence). `npm run e2e:list` lists them without a browser or a stack, which is what the `web` workflow does to keep them loading.
//
// What the environment gives the config and the specs (tests/harness/docker-compose.e2e.yml, service e2e-runner; web/e2e/README.md is the long form). All of it is read lazily, so listing needs none of it:
//   E2E_BASE_URL         the platform's origin as the browser sees it (https; its certificate is self-signed for the run, hence ignoreHTTPSErrors)
//   E2E_ARTIFACTS_DIR    where the run writes everything the workflow uploads: the HTML report, results.json, test-results (traces, screenshots of failures) and shots/ (one picture per main screen)
//   E2E_SHOTS_DIR        the pictures of the specs (web/e2e/support/shots.ts); defaults to <artifacts>/shots
import { join } from 'node:path'
import { defineConfig, devices } from '@playwright/test'

const artifacts = process.env['E2E_ARTIFACTS_DIR'] ?? join(import.meta.dirname, 'e2e-artifacts')

export default defineConfig({
  testDir: './e2e',
  testMatch: '**/*.spec.ts',
  // ONE worker, files in order, no retry: the specs share four users and one stack (their balances, their suppression lists, the sink), and a test that passes only on a retry is a flaky test the stage refuses.
  workers: 1,
  fullyParallel: false,
  retries: 0,
  // a `test.only` left in a spec stops the run instead of silently running one test (the stage also asserts how many tests ran)
  forbidOnly: true,
  timeout: 90_000,
  expect: { timeout: 10_000 },
  // the directory of a run's own files (traces, failure screenshots) and the report are SEPARATE directories: Playwright empties the first at the start of a run
  outputDir: join(artifacts, 'test-results'),
  reporter: [
    ['list'],
    ['html', { outputFolder: join(artifacts, 'report'), open: 'never' }],
    // the stage reads this file, not the exit status alone: a run that ran no test exits 0, and a skipped test is not a pass (tests/harness/e2e/check-report.mjs)
    ['json', { outputFile: join(artifacts, 'results.json') }],
  ],
  use: {
    baseURL: process.env['E2E_BASE_URL'],
    // the stack's TLS edge is a per-run self-signed certificate (tests/harness/e2e/nginx.conf); the page, a fetch of the page and the API requests of a spec all ignore the chain
    ignoreHTTPSErrors: true,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'off',
    acceptDownloads: true,
    locale: 'en-US',
    timezoneId: 'UTC',
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
  // Chromium only: the one browser of the pinned image that the specs and the accessibility checks are written against
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})

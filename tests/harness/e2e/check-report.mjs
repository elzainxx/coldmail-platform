// check-report.mjs — reads the Playwright JSON report of the e2e scope and says whether the run proves anything.
//
//   node check-report.mjs <results.json> <minimum tests> [<listed tests>]
//
// A green exit status of `playwright test` is not enough (false-green class 8: a run of zero tests exits 0): this reads the report the run itself wrote and is RED when
//   - the file is missing, empty or not a Playwright JSON report (no `stats`);
//   - fewer than <minimum> tests ran (the floor; a floor below 1 is refused);
//   - any test failed (`unexpected`), passed only on a retry (`flaky`), or was skipped (`skipped`: a test that cannot run FAILS, it never skips; `test.skip` and `test.fixme` are refused);
//   - <listed tests> is given and the number that ran is not that number (a spec that was listed and did not run is not a pass).
// Prints one line per fact and, last, `check-report: OK` or `check-report: RED`. It prints no test output and no attachment: only counts and the titles of the tests.
// Exit status: 0 green, 1 red, 2 usage.
import { readFileSync } from 'node:fs'

const [file, minText, listedText] = process.argv.slice(2)
const min = Number(minText)
if (!file || !Number.isInteger(min) || min < 1) {
  console.error('usage: check-report.mjs <results.json> <minimum tests, at least 1> [<listed tests>]')
  process.exit(2)
}
const listed = listedText === undefined ? undefined : Number(listedText)
if (listed !== undefined && (!Number.isInteger(listed) || listed < 1)) {
  console.error('check-report: the number of listed tests must be a whole number of at least 1')
  process.exit(2)
}

const problems = []
const red = (m) => problems.push(m)
let report
try {
  const text = readFileSync(file, 'utf8')
  if (text.trim() === '') red(`${file} is empty: the run wrote no report (the instrument matched nothing)`)
  else report = JSON.parse(text)
} catch (e) {
  red(`${file} cannot be read as a report: ${e instanceof Error ? e.message : String(e)}`)
}

if (report) {
  const s = report.stats
  if (!s || ![s.expected, s.unexpected, s.flaky, s.skipped].every((n) => Number.isInteger(n))) {
    red('the report has no `stats` with expected, unexpected, flaky and skipped: it is not a Playwright JSON report')
  } else {
    const titles = []
    const walk = (suite, trail) => {
      const here = suite.title ? [...trail, suite.title] : trail
      for (const spec of suite.specs ?? []) titles.push(`${[...here, spec.title].join(' > ')} [${(spec.tests ?? []).map((t) => t.status).join(',')}]`)
      for (const child of suite.suites ?? []) walk(child, here)
    }
    for (const suite of report.suites ?? []) walk(suite, [])
    console.log(`check-report: ${s.expected} passed, ${s.unexpected} failed, ${s.flaky} flaky, ${s.skipped} skipped, ${titles.length} test(s) named in the report`)
    for (const t of titles) console.log(`  - ${t}`)
    if (s.unexpected > 0) red(`${s.unexpected} test(s) failed`)
    if (s.flaky > 0) red(`${s.flaky} test(s) passed only on a retry (flaky): there are no retries in this scope`)
    if (s.skipped > 0) red(`${s.skipped} test(s) were skipped: a test that cannot run FAILS, it never skips`)
    const ran = s.expected + s.unexpected + s.flaky
    if (ran < min) red(`only ${ran} test(s) ran, the floor is ${min}: the specs did not run (or the floor was raised and a spec was lost)`)
    if (listed !== undefined && ran !== listed) red(`${listed} test(s) were listed and ${ran} ran: a listed spec did not run`)
  }
}

for (const p of problems) console.log(`check-report: RED - ${p}`)
if (problems.length > 0) {
  console.log('check-report: RED')
  process.exit(1)
}
console.log('check-report: OK')

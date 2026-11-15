// Post-build check: the built CSS must contain the generated selector for a Tailwind utility.
//
//   npm run build && npm run check:css
//
// Default utility: min-h-screen (the class on App's root element).
// CHECK_CSS_UTILITY=<name> looks for a different utility instead. Use that for the paired control:
// a utility that no source file uses must NOT be found, and this script must exit 1 for it.
//
// Exit codes: 0 = selector found, 1 = selector absent, 2 = could not run the check
// (no built CSS, empty CHECK_CSS_UTILITY). The codes differ so a control run that exits 2
// because nothing was built cannot pass for "correctly absent".

import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'

const DEFAULT_UTILITY = 'min-h-screen'
const assetsDir = join(import.meta.dirname, '..', 'dist', 'assets')

const utility = (process.env.CHECK_CSS_UTILITY ?? DEFAULT_UTILITY).trim()
if (utility === '') {
  console.error('check-css: CHECK_CSS_UTILITY is set but empty')
  process.exit(2)
}

let cssFiles
try {
  cssFiles = readdirSync(assetsDir).filter((name) => name.endsWith('.css'))
} catch {
  console.error(`check-css: cannot read ${assetsDir} (run "npm run build" first)`)
  process.exit(2)
}
if (cssFiles.length === 0) {
  console.error(`check-css: no .css files in ${assetsDir} (run "npm run build" first)`)
  process.exit(2)
}

// CSS-escape the characters Tailwind escapes in class selectors (":", "[", "/", ...), then
// regex-escape the result. The lookahead stops ".min-h-screen" from matching ".min-h-screen-x".
const cssEscaped = utility.replace(/[^A-Za-z0-9_-]/g, (ch) => `\\${ch}`)
const selector = new RegExp(`\\.${cssEscaped.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}(?![\\w-])`)

const hits = cssFiles.filter((name) => selector.test(readFileSync(join(assetsDir, name), 'utf8')))

if (hits.length === 0) {
  console.error(`check-css: FAIL - selector for "${utility}" not found in: ${cssFiles.join(', ')}`)
  process.exit(1)
}
console.log(`check-css: OK - selector for "${utility}" found in: ${hits.join(', ')}`)

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import type { Plugin } from 'vite'
import { defineConfig } from 'vitest/config'
import { findViolations } from './scripts/check-csp.mjs'

// The platform serves this app under script-src 'self' (Docs/phase-6-spec.md, decision 1). A build whose page holds an inline script or an inline event
// handler would not run there, so the build itself fails on one: the same function as scripts/check-csp.mjs (which CI runs on dist/ as well).
function cspGuard(): Plugin {
  let page = ''
  return {
    name: 'csp-guard',
    apply: 'build',
    configResolved(config) {
      page = resolve(config.root, config.build.outDir, 'index.html')
    },
    closeBundle() {
      const found = findViolations(readFileSync(page, 'utf8'))
      if (found.length > 0) {
        throw new Error(`the built page holds what the Content-Security-Policy refuses: ${found.join('; ')}`)
      }
    },
  }
}

export default defineConfig({
  plugins: [react(), tailwindcss(), cspGuard()],
  build: {
    // The polyfill for <link rel=modulepreload> is a script injected into the entry: the supported browsers need none.
    modulePreload: { polyfill: false },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test-setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
  },
})

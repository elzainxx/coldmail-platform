// Types of scripts/check-csp.mjs for the files that import it (vite.config.ts, the unit test of the check).
export function findViolations(html: string, stats?: { tags: number; scripts: number }): string[]
export function check(distDir: string, log?: Pick<Console, 'log' | 'error'>): 0 | 1 | 2
export function selftest(log?: Pick<Console, 'log' | 'error'>): 0 | 1
export const SELFTEST_CASES: ReadonlyArray<{ name: string; html: string; want: string | null }>
export const SELFTEST_FLOOR: number

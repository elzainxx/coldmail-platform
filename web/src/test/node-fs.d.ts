// The app's type-check does not load @types/node (the browser globals must stay unambiguous). The few tests that read or write a file on disk
// (vitest's css pipeline hands `?raw` an empty string for a .css file; the check of the built page reads a directory) declare the functions they use.
declare module 'node:fs' {
  export function readFileSync(path: string, encoding: 'utf8'): string
  export function writeFileSync(path: string, data: string): void
  export function mkdtempSync(prefix: string): string
  export function rmSync(path: string, options?: { recursive?: boolean; force?: boolean }): void
}
declare module 'node:os' {
  export function tmpdir(): string
}
declare module 'node:path' {
  export function join(...parts: string[]): string
}

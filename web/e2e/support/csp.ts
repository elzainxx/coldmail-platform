// The Content-Security-Policy of the app, as decision 1 of Docs/phase-6-spec.md states it (internal/webapp sets it; this is the specs' own copy of the statement, so that a change on either side is red):
//   default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' https: data:; font-src 'self'; connect-src 'self' <the Supabase origin>; frame-src 'self';
//   base-uri 'none'; form-action 'self'; frame-ancestors 'none'
// No inline script anywhere; inline STYLES are allowed because the sandboxed preview of a user's HTML needs its own (a document in `srcdoc` inherits the embedder's policy).

/** The policy, with the origin GoTrue is reached at (`authOrigin()`). */
export function expectedCsp(supabaseOrigin: string): string {
  return [
    "default-src 'none'",
    "script-src 'self'",
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' https: data:",
    "font-src 'self'",
    `connect-src 'self' ${supabaseOrigin}`,
    "frame-src 'self'",
    "base-uri 'none'",
    "form-action 'self'",
    "frame-ancestors 'none'",
  ].join('; ')
}

/** A policy as a map from directive to its sorted sources: the order of directives and of sources is not part of the policy. */
export function parseCsp(header: string | undefined): Record<string, string[]> {
  const out: Record<string, string[]> = {}
  for (const part of (header ?? '').split(';')) {
    const [name, ...sources] = part.trim().split(/\s+/)
    if (name !== undefined && name !== '') out[name.toLowerCase()] = sources.sort()
  }
  return out
}

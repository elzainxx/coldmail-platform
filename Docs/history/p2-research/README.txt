Phase 2 (kept as a record) - the raw research behind the charge table and the M3 entitlement design in Docs/phase-2-spec.md.

Three read-only passes over the pinned module sources (M1 d039c82, M2 02a71fc, M3 d6ac497) read each module's CODE (not its docs) for the vocabulary a caller can settle from. Every claim cites a file and line in the module repository named in the file. [INFERENCE] marks what was derived instead of read.
  m1-verify-outcomes.txt        the verdict and reason vocabulary, the bulk API, provenance (own / cache / vendor), which unknowns are our own failure, job and item states.
  m2-send-managed-outcomes.txt  the four routes, the six item states, the failure classes and what the caller can and cannot see of them, crash-window duplicates, the system user.
  m3-suspend.txt                the per-user suspend and unsuspend admin routes, idempotency, how a caller can read the state, what stops and what does not, the report vocabulary.
Kept as .txt on purpose (the repository's citation checks read markdown and these notes quote code locations that move when a module changes). A record of what was read, not a living document: internal/charge/rules.tsv is the maintained derivation.

Phase 1, chunk A2.1 — the raw research behind the inventory in Docs/phase-1-spec.md.

Two read-only passes over the pinned module sources (M1 11903d1, M2 02a71fc, M3 d6ac497 at the time) produced these notes: every claim cites a file and line in the module repositories.
  m1-public-and-privileges.txt   what module 1's 15 migrations leave in schema public, M1's runtime privilege matrix (statement -> verb -> granted?), the schema-sensitive code, and the CI gates a change must satisfy.
  m2m3-platform-tables.txt       every statement in modules 2 and 3 that reads, writes or references public.users / public.job_items, the shapes their code requires, and where the two modules differ.
They are kept as .txt on purpose: the repository's citation checks read markdown, and these notes quote code locations that move when a module changes. They are a record of what was read, not a living document; db/module-reads.txt and db/module-statements.txt are the maintained derivations, and scripts/check-module-reads.sh keeps the second one honest.

# Recorded shapes for the shared sender body

`contract/` holds the response bodies of a module-3-SHAPED test profile (omitted-until-set keys in the single report, account detail and `totals_by_account`, no `cancelled`, a feasibility block in
the quote), hand-written from the module's code (`Docs/history/p3-research/m3-api.md`), to be replaced by a recording from the real module in the harness (Phase 3 B6). They exist so that the
tests of the shared body prove it serves both modules, not only module 2; module 3's own adapter keeps its own set under its own `testdata/contract/` and may start from these files. Module 2's
set is `internal/adapters/m2/testdata/contract/`, and the shared body's tests read it from there too.

Ids and times are obviously synthetic and there is no secret in any file.

Request ids and cursors are written in the harness's normalised spelling (a run of `a`, a run of `A`): the fixture check of the drift gate reads a long mixed run of characters as a
secret, and a recording of the real module is normalised the same way.

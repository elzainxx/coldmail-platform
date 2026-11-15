# Phase 0 fault diffs (archive — not run)

Phase 0 proved each gate red on a throwaway branch `fault/p0-<gate>`. The owner authorised deleting those 16 branches at the start of Phase 1; before they went, each branch's fault was saved here as `git diff de42f56 <branch>~1` (the tree one commit before the revert, so fix-up commits are included). `de42f56` is the green head the branches were cut from.

- **Evidence is unchanged:** the red and green run URLs are in [`../../../Docs/phase-0-spec.md`](../../../Docs/phase-0-spec.md). A run URL does not depend on the branch still existing.
- **These patches are not run and are not expected to apply** to today's tree (they were cut from `de42f56`). The Phase 1 regime lives in `ci/faults/<phase>/` (a `.patch` plus a `.expect`, run by the `fault-proofs` workflow). `scripts/check-fault-patches.sh` looks only there.
- **Fifteen files, not sixteen:** `secret` is deliberately absent. Its fault was a token-shaped string, and a stored token-shaped string would trip the secret scan this repository runs on itself. A fault of that kind is *generated* by its `fault-proofs` leg, never stored.

| File | Gate it proved |
|---|---|
| `go-build.patch` | Go compile |
| `unit-test.patch` | Go unit tests |
| `floor-delete.patch` | test-count floor, a test deleted |
| `floor-skip.patch` | test-count floor, a skipped test |
| `image-latest.patch` | digest-pin gate |
| `web-typecheck.patch` | web type-check |
| `web-floor.patch` | web test-count floor |
| `migration-lint.patch` | migration lint |
| `grant-proof.patch` | grant proof (real Postgres) |
| `lock-missing.patch` | pinned-module check |
| `probe-token.patch` | end-to-end probe |
| `exposure.patch` | exposure check |
| `outage.patch` | readiness follows the database |
| `ignore-main.patch` | ignore-file gate |
| `docs-link.patch` | docs / link check |

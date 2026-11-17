-- platform_0008_m3_observed_version.up.sql — a convergence is only as fresh as the version it was observed at. Phase 2 close-out, item D5 (open decision 12).
--
-- THE GAP (OD-12). platform_0006 let converged_version = want_version stand whenever `observed` equalled the wanted state, and said nothing about WHEN module 3 was seen in that state. The reconcile code
-- guards freshness with a compare-and-set on want_version, but the database did not: an observation made at an old version could be re-used for a new one that happens to want the same state. With
-- version 1 wanted active and seen active, version 2 wanted suspended, version 3 wanted active again, a row that says converged_version = 3 and still carries the observation 'active' from version 1 was
-- accepted although module 3 may well have been suspended since (the second version was never delivered). Module 3 now has a suspension READ route (module 3 238bd0a, GET /v1/admin/users/{id}/suspension),
-- so the reconciler reads the state back (internal/entitlements) and the database can demand the freshness.
--
--   * platform.m3_sync.observed_version  bigint, NULL until the first observation: the want_version that was in force when `observed` was recorded (what the observation was compared against).
--   * platform.m3_sync.m3_version        bigint, NULL until a read-back: module 3's OWN transition counter, as the read route answered it. Informational: it is stored so that a counter that goes
--     backwards (a restored module 3 database, another module 3) is visible; the database does not judge it and no rule here depends on it.
--   * The convergence CHECK is REPLACED (m3_sync_converged_only_when_observed, platform_0006, becomes m3_sync_converged_only_when_observed_at_version): converged_version = want_version now needs
--     observed_version = want_version AND observed = the wanted state. The old clause (observed equals the wanted state) is kept inside it. A row that claims convergence has every column of the claim
--     non-NULL: observed_version IS NOT NULL is stated, so a NULL can never make the CHECK evaluate to NULL and pass (CLAUDE.md rule 13).
--   * The entitlement trigger (platform_0006) is NOT changed and does not need to be: it raises want_version, which by itself makes every earlier observation stale (observed_version < want_version),
--     so an observation can never carry over to a new wanted state.
--   * platform_app may update the two new columns (column-level, like the five it already records); want_* and user_id stay the trigger's, and nobody inserts or deletes a row.
--
-- THE ONE-TIME BACKFILL (the only honest one). Every row that exists when this runs was converged, if at all, by the OLD rule: module 3 was seen in the wanted state at the version the row converged at. So
-- observed_version := converged_version for every row that has converged at least once (converged_version >= 1); a converged row (converged_version = want_version) therefore satisfies the new CHECK, which
-- is validated against every row by ADD CONSTRAINT. A row that has never converged (converged_version = 0) has no version to claim and keeps NULL, and m3_version is NULL everywhere: no read-back
-- existed before this migration, and a number cannot be invented for one. For a row behind its want_version the value is the version it last converged at (a later answer of module 3 may have replaced the
-- observation; that row is not converged, the CHECK is not in play for it and its next attempt overwrites both columns). Nothing else in a row is touched.
--
-- Created AS platform_owner (see platform_0002), which owns platform.m3_sync, so the ALTERs, the UPDATE and the GRANT need no further privilege.

SET LOCAL ROLE platform_owner;

ALTER TABLE platform.m3_sync ADD COLUMN observed_version bigint;
ALTER TABLE platform.m3_sync ADD COLUMN m3_version bigint;
COMMENT ON COLUMN platform.m3_sync.observed_version IS 'The want_version that was in force when `observed` was recorded: what the observation was compared against. NULL until the first observation. converged_version = want_version needs observed_version = want_version (m3_sync_converged_only_when_observed_at_version), so an observation made for one wanted state can never confirm another.';
COMMENT ON COLUMN platform.m3_sync.m3_version IS 'Module 3''s own suspension-transition counter as the read route (GET /v1/admin/users/{id}/suspension) last answered it. NULL until a read-back. Informational: stored so a counter that goes backwards is visible; no rule depends on it.';

-- the one-time backfill (see the header): a row that converged under the old rule was observed at the version it converged at
UPDATE platform.m3_sync SET observed_version = converged_version WHERE converged_version >= 1;

-- the convergence rule, replaced: the observation must be of the version the row claims, AND be the wanted state
ALTER TABLE platform.m3_sync DROP CONSTRAINT m3_sync_converged_only_when_observed;
ALTER TABLE platform.m3_sync ADD CONSTRAINT m3_sync_converged_only_when_observed_at_version CHECK (
    converged_version < want_version
    OR (observed_version IS NOT NULL
        AND observed_version = want_version
        AND ((want_suspended AND observed = 'suspended')
             OR (NOT want_suspended AND observed = 'active'))));
COMMENT ON CONSTRAINT m3_sync_converged_only_when_observed_at_version ON platform.m3_sync IS 'D5 (open decision 12): "converged" (converged_version = want_version) may be recorded only while module 3 was observed in the wanted state AT THIS want_version (observed_version = want_version). A stale observation, an observation of another version and a NULL version are all refused.';

COMMENT ON TABLE platform.m3_sync IS 'Delivery of each entitlement to module 3. want_* is raised by a trigger when the entitlement changes; the reconcile loop reads module 3''s suspension state back and records it (observed, at observed_version, with module 3''s own m3_version) and may set converged_version = want_version only while module 3 was observed in the wanted state AT that want_version (m3_sync_converged_only_when_observed_at_version): the platform never reports a state module 3 was not seen to be in.';

-- the reconcile loop records the two new columns with the five it already records (the wanted state stays the trigger's)
GRANT UPDATE (observed_version, m3_version) ON platform.m3_sync TO platform_app;

INSERT INTO platform.schema_migrations (version) VALUES ('0008');

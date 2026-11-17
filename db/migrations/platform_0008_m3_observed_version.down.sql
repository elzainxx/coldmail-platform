-- platform_0008_m3_observed_version.down.sql — undo platform_0008: the observed-version rule goes, the platform_0006 convergence CHECK (observed equals the wanted state, nothing about the version) is back
-- exactly as platform_0006 declared it, and the two columns are dropped (their privileges go with them). Never refuses: the restored CHECK is weaker than the one it replaces, so every row satisfies it.
-- Dropping the columns loses what they recorded (the version each observation was made at and module 3's own counter); the rest of every delivery row is untouched. Rehearsal or a failed deploy only
-- (the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true).
SET LOCAL ROLE platform_owner;

ALTER TABLE platform.m3_sync DROP CONSTRAINT m3_sync_converged_only_when_observed_at_version;
ALTER TABLE platform.m3_sync ADD CONSTRAINT m3_sync_converged_only_when_observed CHECK (
    converged_version < want_version
    OR (want_suspended AND observed = 'suspended')
    OR (NOT want_suspended AND observed = 'active'));

COMMENT ON TABLE platform.m3_sync IS 'Delivery of each entitlement to module 3. want_* is raised by a trigger when the entitlement changes; the reconcile loop records what module 3 answered (observed) and may set converged_version = want_version only while observed equals the wanted state (m3_sync_converged_only_when_observed): the platform never reports a state module 3 was not seen to be in.';

ALTER TABLE platform.m3_sync DROP COLUMN observed_version;
ALTER TABLE platform.m3_sync DROP COLUMN m3_version;

DELETE FROM platform.schema_migrations WHERE version = '0008';

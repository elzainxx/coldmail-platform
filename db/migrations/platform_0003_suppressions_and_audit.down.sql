-- platform_0003_suppressions_and_audit.down.sql — remove the suppression list and the admin audit log. Destroys both (a rehearsal or a failed deploy only; the migrator refuses `down` unless allowed).
SET LOCAL ROLE platform_owner;

DROP TABLE platform.admin_audit;
DROP TABLE platform.suppressions;

DELETE FROM platform.schema_migrations WHERE version = '0003';

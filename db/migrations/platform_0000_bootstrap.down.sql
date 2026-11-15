-- platform_0000_bootstrap.down.sql — reverse of the bootstrap (dev/CI only; the runner refuses it
-- unless PLATFORM_MIGRATE_ALLOW_DOWN=true, and refuses while a higher version is applied).

-- Bookkeeping delete (explicit, though the CASCADE below also removes the table).
DELETE FROM platform.schema_migrations WHERE version = '0000';

DROP SCHEMA IF EXISTS platform CASCADE;

-- Drop the cluster-global roles BEST-EFFORT. Roles are shared across databases; if one still owns
-- objects or holds privileges elsewhere, DROP ROLE raises dependent_objects_still_exist (2BP01).
-- That is legitimate — the role is still in use — so it is retained rather than aborting the down.
DO $$
BEGIN
    BEGIN
        DROP ROLE IF EXISTS platform_admin;
    EXCEPTION WHEN dependent_objects_still_exist THEN
        RAISE NOTICE 'platform_admin retained: still referenced in another database';
    END;
    BEGIN
        DROP ROLE IF EXISTS platform_app;
    EXCEPTION WHEN dependent_objects_still_exist THEN
        RAISE NOTICE 'platform_app retained: still referenced in another database';
    END;
    BEGIN
        DROP ROLE IF EXISTS platform_owner;
    EXCEPTION WHEN dependent_objects_still_exist THEN
        RAISE NOTICE 'platform_owner retained: still referenced in another database';
    END;
END
$$;

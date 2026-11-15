-- platform_0001_public_tables.down.sql — drop public.job_items and public.users.
-- This FAILS, deliberately and with SQLSTATE 2BP01, once any module has migrated: their foreign keys depend on these tables, and dropping them would destroy module history.
-- Roll the modules back first (or do not roll this back).
DROP TABLE public.job_items;
DROP TABLE public.users;
DO $$
BEGIN
    EXECUTE format('ALTER ROLE platform_app IN DATABASE %I RESET timezone', current_database());
END
$$;
DELETE FROM platform.schema_migrations WHERE version = '0001';

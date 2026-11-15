-- platform_0000_bootstrap.up.sql — the platform schema boundary: roles, schema, tracking table.
-- NO business tables. Those are Phase 1 (docs: Docs/coldmail-platform-build-plan.md, P1).
--
-- Run by the migrator as a role that may CREATE ROLE and re-own objects (a superuser in the
-- harness; which role runs it on the real Supabase project is an owner decision — see
-- Docs/phase-0-spec.md "Open decisions" 2). The runtime never runs migrations.
--
-- Roles (cluster-global, created idempotently):
--   platform_owner  NOLOGIN  owns the schema and every object in it.
--   platform_app    LOGIN    the service's runtime identity. Its password is set OUT OF BAND by
--                            `platform-migrate set-app-password` — never in SQL, never in git.
--   platform_admin  NOLOGIN  break-glass: no members today (the same shape as module 1's cp_admin).
--                            Granted to a person's login only by an explicit, audited act.
--
-- Deliberately NOT done here: ALTER DEFAULT PRIVILEGES. Modules 2 and 3 set it, but their own
-- migration 0012 records that it never fires for tables the migrator creates and then re-owns
-- (default privileges apply to objects CREATED BY the named role). Here every table gets explicit
-- grants in the migration that creates it, and the grants test fails on any table without them.

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'platform_owner') THEN
        CREATE ROLE platform_owner NOLOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'platform_app') THEN
        CREATE ROLE platform_app LOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'platform_admin') THEN
        CREATE ROLE platform_admin NOLOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB;
    END IF;
END
$$;

CREATE SCHEMA IF NOT EXISTS platform AUTHORIZATION platform_owner;
REVOKE ALL ON SCHEMA platform FROM PUBLIC;
GRANT USAGE ON SCHEMA platform TO platform_app, platform_admin;

CREATE TABLE IF NOT EXISTS platform.schema_migrations (
    version    TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE platform.schema_migrations OWNER TO platform_owner;
REVOKE ALL ON platform.schema_migrations FROM PUBLIC;
GRANT SELECT ON platform.schema_migrations TO platform_app, platform_admin;

INSERT INTO platform.schema_migrations (version) VALUES ('0000');

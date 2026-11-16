-- preprovision.sql — the REQUIRED install step for modules 2 and 3 on Supabase (the owner's ruling OD-7, Phase 2; it was a proposal in Phase 1). No module is changed. Run ONCE by the migrator (the non-superuser `postgres`), BEFORE the module chains; Docs/install.md names it as step 2, and CI runs it exactly as an install does.
--
-- WHY. Modules 2 and 3 may not be changed from this repository. Their bootstraps (send_managed_0000 / send_byo_0000) create their owner role under a guard
--   IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '<module>_owner') THEN CREATE ROLE ...
-- written for "managed / out-of-band role provisioning", then `CREATE SCHEMA ... AUTHORIZATION <module>_owner` and `ALTER DEFAULT PRIVILEGES FOR ROLE <module>_owner ...`.
-- On Supabase the migrator `postgres` is not a superuser. On PostgreSQL 16+ the CREATOR of a role is given only ADMIN OPTION on it, with INHERIT FALSE and SET FALSE, so both
-- statements fail for the migrator ("must be able to SET ROLE <owner>", "permission denied to change default privileges"; measured on supabase/postgres 17.12, SQLSTATE 42501).
-- A role holding ADMIN OPTION on another may grant it to itself WITH INHERIT TRUE, SET TRUE, and then both statements work. This file does exactly that, for the two owners,
-- BEFORE the modules' own bootstraps run; their role-creation guards then find the roles in place and skip. (PostgreSQL 15 and earlier have no per-grant SET / INHERIT options:
-- a plain GRANT of the role to the migrator does it.)
--
-- Idempotent: a role that exists is not created again; a grant that already gives USAGE and SET is not repeated. Roles are cluster-wide, so a role that an earlier run created
-- WITHOUT this grant (the baseline pass of tests/supabase) is found and granted here. Run by tests/supabase/run.sh --preprovision with ON_ERROR_STOP=1: any failure is red.
-- The 'SET' privilege name exists only on PostgreSQL 16+, so it appears only inside the branch that is reached on 16+ (PL/pgSQL plans an expression when it is first run).

DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['send_managed_owner', 'send_byo_owner'] LOOP
        IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('CREATE ROLE %I NOLOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB', r);
        END IF;
        IF current_setting('server_version_num')::int >= 160000 THEN
            IF NOT (pg_has_role(current_user, r, 'USAGE') AND pg_has_role(current_user, r, 'SET')) THEN
                EXECUTE format('GRANT %I TO %I WITH INHERIT TRUE, SET TRUE', r, current_user);
            END IF;
        ELSE
            IF NOT pg_has_role(current_user, r, 'USAGE') THEN
                EXECUTE format('GRANT %I TO %I', r, current_user);
            END IF;
        END IF;
    END LOOP;
END
$$;

-- the file proves its own effect: afterwards the migrator must hold both roles with USAGE (inherit) and, on 16+, SET
DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['send_managed_owner', 'send_byo_owner'] LOOP
        IF NOT pg_has_role(current_user, r, 'USAGE') THEN
            RAISE EXCEPTION 'preprovision: % still lacks USAGE (inherit) on role %', current_user, r;
        END IF;
        IF current_setting('server_version_num')::int >= 160000 THEN
            IF NOT pg_has_role(current_user, r, 'SET') THEN
                RAISE EXCEPTION 'preprovision: % still cannot SET ROLE %', current_user, r;
            END IF;
        END IF;
    END LOOP;
END
$$;

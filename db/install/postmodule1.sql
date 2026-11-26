-- postmodule1.sql — the install step AFTER module 1's migrations (Docs/install.md, step 3b; Phase 4, chunk B16). No module is changed. Run by the migrator (the non-superuser `postgres`), once module 1's own migrations have run
-- and before the platform serves a module 1 job. preprovision.sql is its mirror image: that file runs BEFORE the modules' migrations, this one AFTER module 1's.
--
-- WHAT IT BUILDS. One function, platform.provision_m1_caller(p_user_id uuid, p_key_sha256 bytea) RETURNS text ('created' or 'exists'), that writes the ONE row module 1 needs to recognise a user's key. Module 1 has no API that
-- mints a key: verify.api_callers is written only by its privileged identity (its migration 0010: the runtime role has SELECT and nothing else). The platform service runs as platform_app (no DDL, no module rights) and
-- must never hold module 1's admin credential, so the privilege lives in this function and nowhere else. The service calls it at a user's first module 1 job (internal/tenant, M1CallerEnsurer); no operator step per user.
--
--   * SECURITY DEFINER, OWNED by verify_admin (module 1's grant-admin: migration 0010 gives it ALL PRIVILEGES on api_callers, migration 0016 renamed cp_admin to it and left it NOLOGIN), search_path pinned to pg_catalog, pg_temp
--     (every name below is schema-qualified or in pg_catalog: a shadowing object in a schema the caller controls can never be reached).
--   * It writes ONE kind of row: caller_id is the constant shape 'platform-user-' || user (the caller cannot choose another), not_before an hour back (module 1 compares it with ITS clock), no expiry, user_id the user, status
--     and created_at the table's defaults. The insert is ON CONFLICT (key_sha256) DO NOTHING; the row for the digest is then READ BACK and the call RAISES when it belongs to another user, has no owner, or is outside its validity
--     window (the same judgement as tenant.ProvisionM1CallerVersion: a silent DO NOTHING must never hide an ownerless key, which would accept jobs nobody can read). It cannot UPDATE, DELETE, TRUNCATE or read anything else.
--   * It refuses a NULL user, the all-zero system user and a digest that is not 32 bytes (SQLSTATE and CONSTRAINT name in the function body: Go maps them by the name).
--   * REVOKE ALL FROM PUBLIC; EXECUTE for platform_app and nobody else. The grants CONVERGE on every run: whatever else is found on the function (a default privilege of the image, a hand-made grant) is revoked, and the file
--     proves its own effect at the end (owner, SECURITY DEFINER, the pinned path, the exact ACL, who can execute it, that verify_admin holds nothing on schema platform afterwards).
--
-- THREAT MODEL (also the comment on the function). platform_app already holds the derivation secret, so it can compute every user's key and already acts as any user at module 1; the function gives it no power it lacks. What it
-- removes is the need for module 1's ADMIN credential in the service, whose reach is every table of module 1. What the function still allows a compromised service: to register a key of its own choosing for a user id (it could
-- anyway use the derived one) and to add rows without bound. What it cannot: change or remove a row, register a key with no owner or for the system user, choose a caller_id or a window, or touch another table. Retiring a key
-- (rotation step 3) stays an operator action through the privileged identity (tenant.RetireM1CallerVersion).
--
-- HOW IT RUNS AS A NON-SUPERUSER (what is REASONED here; the first runner run measures it, and the supabase workflow's FIXED leg and the harness stage 4 run this file twice as `postgres` on the pinned image).
--   1. On PostgreSQL 16+ the CREATOR of a role holds only ADMIN OPTION on it. verify_admin was created by the migrator (module 1's migration 0005), so the migrator may grant it to itself WITH INHERIT TRUE, SET TRUE, exactly as
--      preprovision.sql does for the two send owners. (PostgreSQL 15 and earlier have no per-grant options: a plain GRANT does it. The 'SET' option name exists only on 16+, so it appears only inside the branch reached there.)
--   2. A function can be created in schema platform only by a role that holds CREATE on it. platform_owner owns the schema and the migrator holds it (platform_0000), so, as platform_owner, the file grants verify_admin USAGE and
--      CREATE on schema platform FOR THE LENGTH OF THE TRANSACTION (USAGE because GRANT/COMMENT ... ON FUNCTION platform.x look the name up through the schema), switches to verify_admin (SET LOCAL ROLE, allowed by step 1) to
--      create the function, set its grants and comment it, then, as platform_owner again, takes both privileges back. The whole file is ONE transaction: any failure rolls the temporary grants back with everything else.
-- Idempotent and re-runnable (CREATE OR REPLACE, grants that converge, a membership that is granted only when missing); ON_ERROR_STOP-clean (run with psql -v ON_ERROR_STOP=1: any failure is red).

BEGIN;

-- 1. what must already be there: module 1 migrated (with the columns this function writes) and the three roles it names
DO $$
DECLARE
    r    text;
    cols integer;
BEGIN
    IF to_regnamespace('platform') IS NULL THEN
        RAISE EXCEPTION 'postmodule1: schema platform does not exist; the platform migrations (Docs/install.md step 3.1) must run before this step' USING ERRCODE = '3F000';
    END IF;
    IF to_regclass('verify.api_callers') IS NULL THEN
        RAISE EXCEPTION 'postmodule1: verify.api_callers does not exist; module 1''s migrations (Docs/install.md step 3.2) must run before this step' USING ERRCODE = '42P01';
    END IF;
    FOREACH r IN ARRAY ARRAY['platform_owner', 'platform_app', 'verify_admin'] LOOP
        IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = r) THEN
            RAISE EXCEPTION 'postmodule1: role % does not exist (module 1''s migrations create verify_admin, the platform''s create platform_owner and platform_app)', r USING ERRCODE = '42704';
        END IF;
    END LOOP;
    SELECT count(*) INTO cols FROM pg_attribute
     WHERE attrelid = 'verify.api_callers'::regclass AND NOT attisdropped AND attname IN ('caller_id', 'key_sha256', 'not_before', 'not_after', 'user_id');
    IF cols <> 5 THEN
        RAISE EXCEPTION 'postmodule1: verify.api_callers has % of the 5 columns this step writes and reads (caller_id, key_sha256, not_before, not_after, user_id): module 1''s schema is not the one this step was written for', cols USING ERRCODE = '42703';
    END IF;
    IF NOT (has_table_privilege('verify_admin', 'verify.api_callers', 'INSERT') AND has_table_privilege('verify_admin', 'verify.api_callers', 'SELECT')) THEN
        RAISE EXCEPTION 'postmodule1: verify_admin cannot INSERT into and SELECT from verify.api_callers: the function would have nothing to run with (module 1''s migration 0010 grants it ALL PRIVILEGES)' USING ERRCODE = '42501';
    END IF;
END
$$;

-- 2. the migrator may act as verify_admin: the same grant, in the same shape, as preprovision.sql gives it for the two send owners
DO $$
BEGIN
    IF current_setting('server_version_num')::int >= 160000 THEN
        IF NOT (pg_has_role(current_user, 'verify_admin', 'USAGE') AND pg_has_role(current_user, 'verify_admin', 'SET')) THEN
            EXECUTE format('GRANT verify_admin TO %I WITH INHERIT TRUE, SET TRUE', current_user);
        END IF;
    ELSIF NOT pg_has_role(current_user, 'verify_admin', 'USAGE') THEN
        EXECUTE format('GRANT verify_admin TO %I', current_user);
    END IF;
EXCEPTION WHEN insufficient_privilege THEN
    RAISE EXCEPTION 'postmodule1: % may not grant itself module 1''s role verify_admin (it needs ADMIN OPTION on it, which the role that created it holds): run this step as the role that ran module 1''s migrations', current_user USING ERRCODE = '42501';
END
$$;

-- 3. for the length of this transaction: verify_admin may create in (and name objects of) schema platform
SET LOCAL ROLE platform_owner;
GRANT USAGE, CREATE ON SCHEMA platform TO verify_admin;

-- 4. the function, created by its owner
SET LOCAL ROLE verify_admin;

CREATE OR REPLACE FUNCTION platform.provision_m1_caller(p_user_id uuid, p_key_sha256 bytea)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_inserted bigint;
    v_owner    uuid;
    v_valid    boolean;
BEGIN
    IF p_user_id IS NULL THEN
        RAISE EXCEPTION 'provision_m1_caller: the user id is NULL (a module 1 key with no owner accepts jobs that nobody can read)'
            USING ERRCODE = '22004', CONSTRAINT = 'm1_caller_user_required';
    END IF;
    IF p_user_id = '00000000-0000-0000-0000-000000000000'::uuid THEN
        RAISE EXCEPTION 'provision_m1_caller: the system user is never a module 1 caller'
            USING ERRCODE = '22023', CONSTRAINT = 'm1_caller_not_the_system_user';
    END IF;
    IF p_key_sha256 IS NULL OR octet_length(p_key_sha256) <> 32 THEN
        RAISE EXCEPTION 'provision_m1_caller: the key digest must be exactly 32 bytes (a SHA-256)'
            USING ERRCODE = '22023', CONSTRAINT = 'm1_caller_digest_is_32_bytes';
    END IF;

    INSERT INTO verify.api_callers (caller_id, key_sha256, not_before, user_id)
    VALUES ('platform-user-' || p_user_id::text, p_key_sha256, now() - interval '1 hour', p_user_id)
    ON CONFLICT (key_sha256) DO NOTHING;
    GET DIAGNOSTICS v_inserted = ROW_COUNT;

    SELECT c.user_id, (c.not_before <= now() AND (c.not_after IS NULL OR c.not_after > now()))
      INTO v_owner, v_valid
      FROM verify.api_callers c
     WHERE c.key_sha256 = p_key_sha256;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'provision_m1_caller: the row of user % is not there after the insert', p_user_id
            USING ERRCODE = '55000', CONSTRAINT = 'm1_caller_row_after_insert';
    END IF;
    IF v_owner IS DISTINCT FROM p_user_id OR NOT v_valid THEN
        RAISE EXCEPTION 'provision_m1_caller: a module 1 caller row for the key of user % exists but does not act for that user (another owner, no owner, or outside its validity window); nothing was changed', p_user_id
            USING ERRCODE = '23505', CONSTRAINT = 'm1_caller_row_acts_for_the_user';
    END IF;
    RETURN CASE WHEN v_inserted = 1 THEN 'created' ELSE 'exists' END;
END
$fn$;

-- the grants converge on every run: whatever is on the function besides its owner is taken back, then the one grant is made
DO $$
DECLARE
    g   oid;
    who text;
BEGIN
    FOR g IN
        SELECT a.grantee
          FROM pg_proc p CROSS JOIN LATERAL aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) a
         WHERE p.oid = 'platform.provision_m1_caller(uuid, bytea)'::regprocedure AND a.grantee <> p.proowner
    LOOP
        IF g = 0 THEN
            REVOKE ALL ON FUNCTION platform.provision_m1_caller(uuid, bytea) FROM PUBLIC;
        ELSE
            SELECT rolname INTO who FROM pg_roles WHERE oid = g;
            EXECUTE format('REVOKE ALL ON FUNCTION platform.provision_m1_caller(uuid, bytea) FROM %I', who);
        END IF;
    END LOOP;
    GRANT EXECUTE ON FUNCTION platform.provision_m1_caller(uuid, bytea) TO platform_app;
END
$$;

COMMENT ON FUNCTION platform.provision_m1_caller(uuid, bytea) IS
    'Phase 4 B16. The one door through which the platform service registers a user''s module 1 caller key: SECURITY DEFINER, owned by verify_admin (module 1''s grant-admin), search_path pinned. It INSERTs one kind of row into verify.api_callers (caller_id platform-user-<user>, not_before an hour back, no expiry, user_id the user) ON CONFLICT (key_sha256) DO NOTHING, reads the row back and raises when it belongs to another user, has no owner or is outside its window; returns created or exists. It cannot UPDATE or DELETE anything. Executable by platform_app only. Threat model: platform_app already holds the derivation secret and so already acts as any user at module 1; this function adds no power it lacks and removes the need for module 1''s admin credential in the service.';

-- 5. take the temporary privileges back
SET LOCAL ROLE platform_owner;
REVOKE USAGE, CREATE ON SCHEMA platform FROM verify_admin;
RESET ROLE;

-- 6. the file proves its own effect: the function is what it claims to be, only platform_app can execute it, and verify_admin holds nothing on schema platform any more
DO $$
DECLARE
    f   regprocedure := 'platform.provision_m1_caller(uuid, bytea)'::regprocedure;
    p   pg_proc%ROWTYPE;
    acl text;
    r   text;
BEGIN
    SELECT * INTO p FROM pg_proc WHERE oid = f;
    IF pg_get_userbyid(p.proowner) <> 'verify_admin' THEN
        RAISE EXCEPTION 'postmodule1: platform.provision_m1_caller is owned by %, want verify_admin', pg_get_userbyid(p.proowner);
    END IF;
    IF NOT p.prosecdef THEN
        RAISE EXCEPTION 'postmodule1: platform.provision_m1_caller is not SECURITY DEFINER';
    END IF;
    IF p.proconfig IS NULL OR NOT ('search_path=pg_catalog, pg_temp' = ANY (p.proconfig)) THEN
        RAISE EXCEPTION 'postmodule1: platform.provision_m1_caller does not pin search_path = pg_catalog, pg_temp (proconfig %)', p.proconfig;
    END IF;
    SELECT string_agg(g.label, ', ' ORDER BY g.label) INTO acl
      FROM (SELECT (CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END) || ':' || a.privilege_type || ':' || a.is_grantable::text AS label
              FROM aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) a) g;
    IF acl IS DISTINCT FROM 'platform_app:EXECUTE:false, verify_admin:EXECUTE:false' THEN
        RAISE EXCEPTION 'postmodule1: the privileges on platform.provision_m1_caller are [%], want exactly [platform_app:EXECUTE:false, verify_admin:EXECUTE:false] (the owner and platform_app)', acl;
    END IF;
    FOREACH r IN ARRAY ARRAY['verify_app', 'anon', 'authenticated', 'service_role'] LOOP
        IF EXISTS (SELECT FROM pg_roles WHERE rolname = r) AND has_function_privilege(r::name, f::oid, 'EXECUTE') THEN
            RAISE EXCEPTION 'postmodule1: role % can execute platform.provision_m1_caller, and only platform_app may', r;
        END IF;
    END LOOP;
    IF NOT has_schema_privilege('platform_app', 'platform', 'USAGE') OR has_schema_privilege('platform_app', 'platform', 'CREATE') THEN
        RAISE EXCEPTION 'postmodule1: platform_app must hold USAGE and not CREATE on schema platform';
    END IF;
    IF has_schema_privilege('verify_admin', 'platform', 'USAGE') OR has_schema_privilege('verify_admin', 'platform', 'CREATE') THEN
        RAISE EXCEPTION 'postmodule1: verify_admin still holds USAGE or CREATE on schema platform: the temporary privileges were not taken back';
    END IF;
END
$$;

COMMIT;

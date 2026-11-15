-- platform_0001_public_tables.up.sql — the two platform-owned tables that modules 2 and 3 read in schema public: public.users and public.job_items. Phase 1, chunk B2.
--
-- THE SHAPE IS NOT OURS TO CHOOSE. Modules 2 and 3 are built and tested against these two tables, so each column below is here because module CODE reads or writes it (Docs/phase-1-spec.md,
-- "Inventory": every statement traced to a file and line). The declared shape is in db/module-reads.txt and a test compares it with the catalog; scripts/check-module-reads.sh fails when a
-- pinned module's code starts referencing something the declaration does not list. The rules that follow from module code:
--   * users.id and job_items.id are uuid PRIMARY KEYs with defaults (foreign-key targets of 7 + 11 module constraints; module 3's test-send INSERT omits job_items.id).
--   * job_items.campaign_id and user_id are BOTH uuid NOT NULL (module 3 binds one parameter to both columns) and campaign_id has NO foreign key and no check: module 3's test-send
--     writes the tenant's own id there.
--   * recipient_email, rendered_subject, rendered_body are NOT NULL: a NULL surfaces in the modules as an opaque 500 (SQLSTATE 23502 is not mapped), not a clean 4xx.
--   * status must ACCEPT 'pending' (the only value any module writes) and is never read by a module.
--   * EVERY OTHER column on job_items must be nullable or defaulted: module 3's INSERT supplies exactly six columns.
--   * The module roles' grants on these tables (SELECT on both for module 2 and 3, INSERT on job_items for module 3) are issued BY THE MODULE MIGRATIONS and asserted exactly by their own
--     tests. The platform grants the module roles nothing here and must never add a grant, a row-level-security policy or a trigger that changes what they can do.
--   * Nothing is ever hard-deleted: module foreign keys CASCADE from both tables (a deleted job_item destroys the module's job, events and send log; module 3's accounts RESTRICT a
--     deleted user). The platform therefore grants DELETE on neither (a grants test pins it).
--
-- THE SYSTEM USER. Module 2 hard-requires a row for the all-zero uuid in public.users (shared pools and accounts answer 409 without it). It has no Supabase login, so users.id cannot be a
-- plain foreign key to auth.users: auth_user_id is the (nullable, UNIQUE) foreign key, and users_auth_link says every row either IS its auth user (id = auth_user_id) or is that one system row.
--
-- PRECONDITION: schema auth with table auth.users (Supabase Auth). The harness creates a minimal fixture of it (tests/harness/fixtures/auth-users.sql); on Supabase it is the real one.
-- Run by the migrator (a superuser in the harness; `postgres` on Supabase, which platform_0000 made able to SET ROLE to platform_owner).

DO $$
BEGIN
    IF to_regclass('auth.users') IS NULL THEN
        RAISE EXCEPTION 'platform_0001: auth.users does not exist. Supabase Auth must be deployed first (the harness creates a fixture: tests/harness/fixtures/auth-users.sql).'
            USING ERRCODE = '55000';
    END IF;
END
$$;

-- The service's sessions are UTC (every instant in this schema is timestamptz; this keeps casts and date_trunc unambiguous). Scoped to THIS DATABASE (IN DATABASE): roles are cluster-wide, and an
-- unscoped setting would be changed by a migration or a rollback in any other database of the cluster.
DO $$
BEGIN
    EXECUTE format('ALTER ROLE platform_app IN DATABASE %I SET timezone = %L', current_database(), 'UTC');
END
$$;

CREATE TABLE public.users (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    auth_user_id uuid        UNIQUE REFERENCES auth.users (id) ON DELETE RESTRICT,
    email        text        NOT NULL,
    is_active    boolean     NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- COALESCE matters: a CHECK that evaluates to NULL PASSES, so `auth_user_id = id` alone would admit a row with no auth link at all.
    CONSTRAINT users_auth_link CHECK (COALESCE(auth_user_id = id, false) OR (id = '00000000-0000-0000-0000-000000000000'::uuid AND auth_user_id IS NULL)),
    CONSTRAINT users_email_normalised CHECK (email <> '' AND email = btrim(email) AND email = lower(email))
);
CREATE UNIQUE INDEX users_email_key ON public.users (email);

COMMENT ON TABLE public.users IS 'The tenant is a person: one login = one row = one set of balances. id = Supabase auth.users.id (except the all-zero system user module 2 requires). Read by modules 2 and 3 (id only; existence checks and foreign keys). Never hard-deleted.';
COMMENT ON COLUMN public.users.auth_user_id IS 'Foreign key to auth.users. NULL only for the system user (id all zeros): users_auth_link.';
COMMENT ON COLUMN public.users.is_active IS 'The platform''s own switch for a login that may no longer act; no module reads it.';

-- The reserved system user (module 2: shared pools/accounts). Inactive: it can never act as a person.
INSERT INTO public.users (id, auth_user_id, email, is_active)
VALUES ('00000000-0000-0000-0000-000000000000', NULL, 'system@platform.invalid', false);

CREATE TABLE public.job_items (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id      uuid        NOT NULL,
    user_id          uuid        NOT NULL REFERENCES public.users (id) ON DELETE RESTRICT,
    recipient_email  text        NOT NULL,
    rendered_subject text        NOT NULL,
    rendered_body    text        NOT NULL,
    reply_to         text,
    status           text        NOT NULL DEFAULT 'pending',
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT job_items_status_not_blank CHECK (btrim(status) <> '')
);
CREATE INDEX job_items_user_id_idx ON public.job_items (user_id);
CREATE INDEX job_items_campaign_id_idx ON public.job_items (campaign_id);

COMMENT ON TABLE public.job_items IS 'One rendered message per recipient. Read by modules 2 and 3 (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, reply_to); written by module 3 test-send (six columns). campaign_id has NO foreign key on purpose (module 3 test-send writes the user id there). Every column not read by a module must stay nullable or defaulted. Never hard-deleted.';
COMMENT ON COLUMN public.job_items.status IS 'Written as ''pending'' by module 3, never read by a module. The platform owns the vocabulary; it must keep accepting ''pending''.';

-- Ownership: platform_owner owns every object the platform creates. The migrator may SET ROLE to it (platform_0000).
ALTER TABLE public.users     OWNER TO platform_owner;
ALTER TABLE public.job_items OWNER TO platform_owner;

-- Privileges. Nothing to PUBLIC, and nothing to Supabase's API roles: a table created in public on a Supabase project is otherwise readable through the Data API by anon and authenticated.
REVOKE ALL ON public.users, public.job_items FROM PUBLIC;
DO $$
DECLARE r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['anon', 'authenticated', 'service_role'] LOOP
        IF EXISTS (SELECT FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('REVOKE ALL ON public.users, public.job_items FROM %I', r);
        END IF;
    END LOOP;
END
$$;

-- The platform's own runtime: read, create and update, never delete. platform_admin is break-glass and read-only.
GRANT SELECT, INSERT, UPDATE ON public.users TO platform_app;
GRANT SELECT                 ON public.users TO platform_admin;
GRANT SELECT, INSERT, UPDATE ON public.job_items TO platform_app;
GRANT SELECT                 ON public.job_items TO platform_admin;

INSERT INTO platform.schema_migrations (version) VALUES ('0001');

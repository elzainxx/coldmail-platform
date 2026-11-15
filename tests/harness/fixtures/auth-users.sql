-- FIXTURE ONLY — a minimal stand-in for Supabase Auth's auth.users, so platform_0001 (whose public.users.auth_user_id references it) can run on stock Postgres.
-- On a Supabase project auth.users is the real table (GoTrue's): this fixture is NOT its shape beyond what the platform reads — id (uuid primary key) and email. Every harness run and every
-- scratch database in the integration tests applies this file first; docs/phase-1-spec.md lists "the real auth schema" under what the harness does not check.
CREATE SCHEMA IF NOT EXISTS auth;
CREATE TABLE IF NOT EXISTS auth.users (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email      text,
    created_at timestamptz DEFAULT now()
);

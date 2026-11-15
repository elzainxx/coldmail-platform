-- platform_0003_suppressions_and_audit.up.sql — the per-user suppression list and the admin audit log. Phase 1, chunk B4.
--
--   * suppressions: addresses a user must never be sent to, applied to every future job. The address is stored in module 1's frozen `norm/v1` form (trim; exactly one @; domain: one trailing
--     dot stripped, IDNA ToASCII, lower-case, NFC; local part: NFC and lower-case; NO dot-stripping and NO +tag folding) so a suppressed address and a verified address are the SAME address.
--     The Go side produces the form (P5/P6); the database REFUSES whatever SQL can detect: untrimmed, mixed case, not NFC, not exactly one @, an empty side, a domain with anything but
--     [a-z0-9.-] or a leading/trailing dot or hyphen, longer than 254 characters.
--   * admin_audit: one row per admin action AND per admin view of user content (entry_type 'content_view'), against a NAMED admin. Append-only like the ledger.
--
-- Created AS platform_owner (see platform_0002). NULL TRAP: a CHECK that evaluates to NULL passes; every constraint here is over NOT NULL columns or says IS NOT NULL.

SET LOCAL ROLE platform_owner;

CREATE TABLE platform.suppressions (
    user_id    uuid        NOT NULL REFERENCES public.users (id) ON DELETE RESTRICT,
    email      text        NOT NULL,
    source     text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT suppressions_pk PRIMARY KEY (user_id, email),
    CONSTRAINT suppressions_source_vocabulary CHECK (source IN ('unsubscribe', 'manual', 'import')),
    CONSTRAINT suppressions_email_norm_v1 CHECK (
        email = btrim(email)
        AND email = lower(email)
        AND email IS NFC NORMALIZED
        AND octet_length(email) <= 254                                    -- BYTES, as the address limit is stated (length() would count a two-byte letter once)
        AND length(email) - length(replace(email, '@', '')) = 1          -- exactly one @
        AND email ~ '^[^@[:space:]]+@[a-z0-9]([a-z0-9.-]*[a-z0-9])?$'    -- non-empty local part without space; ASCII domain (punycode), no leading/trailing dot or hyphen
        AND split_part(email, '@', 2) !~ '\.\.'                           -- no empty label
        AND split_part(email, '@', 2) !~ '(^|\.)-|-(\.|$)'                -- no label that starts or ends with a hyphen
    )
);
COMMENT ON TABLE platform.suppressions IS 'Per-user suppressed recipient addresses, in module 1''s frozen norm/v1 form, applied to every future job. Hard bounces are NOT auto-suppressed (locked decision). Rows are never updated; nothing deletes one until a removal flow is designed.';
COMMENT ON COLUMN platform.suppressions.email IS 'norm/v1: trimmed, one @, local part NFC + lower-case (dots and +tags kept), domain IDNA-ASCII lower-case without a trailing dot. The database refuses what SQL can check; the Go side produces the form.';

CREATE TABLE platform.admin_audit (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    admin_id       uuid        NOT NULL REFERENCES platform.admins (id) ON DELETE RESTRICT,
    entry_type     text        NOT NULL,
    action         text        NOT NULL,
    target_type    text        NOT NULL,
    target_id      text        NOT NULL,
    target_user_id uuid        REFERENCES public.users (id) ON DELETE RESTRICT,
    reason         text        NOT NULL,
    details        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT admin_audit_entry_type_vocabulary CHECK (entry_type IN ('action', 'content_view')),
    CONSTRAINT admin_audit_action_not_blank CHECK (btrim(action) <> ''),
    CONSTRAINT admin_audit_target_not_blank CHECK (btrim(target_type) <> '' AND btrim(target_id) <> ''),
    CONSTRAINT admin_audit_reason_not_blank CHECK (btrim(reason) <> ''),
    CONSTRAINT admin_audit_details_is_an_object CHECK (jsonb_typeof(details) = 'object'),
    -- looking at a user's content is always about a user
    CONSTRAINT admin_audit_content_view_names_a_user CHECK (entry_type <> 'content_view' OR target_user_id IS NOT NULL)
);
CREATE INDEX admin_audit_admin_idx ON platform.admin_audit (admin_id, id DESC);
CREATE INDEX admin_audit_target_user_idx ON platform.admin_audit (target_user_id, id DESC) WHERE target_user_id IS NOT NULL;
COMMENT ON TABLE platform.admin_audit IS 'Append-only. Every admin action and every admin view of user content, against a NAMED admin (the modules know only one operator key, so this is the only place that knows which admin acted). entry_type content_view records that content was looked at; message bodies are viewable for 7 days, outcomes forever (locked decision).';

CREATE TRIGGER admin_audit_append_only BEFORE UPDATE OR DELETE ON platform.admin_audit FOR EACH ROW EXECUTE FUNCTION platform.reject_mutation();
CREATE TRIGGER admin_audit_no_truncate BEFORE TRUNCATE ON platform.admin_audit FOR EACH STATEMENT EXECUTE FUNCTION platform.reject_mutation();

-- Privileges. A user's suppression list grows through the application and is read by it; the audit log is written by the application AND by the break-glass role.
GRANT SELECT, INSERT ON platform.suppressions TO platform_app;
GRANT SELECT ON platform.suppressions TO platform_admin;
GRANT SELECT, INSERT ON platform.admin_audit TO platform_app, platform_admin;

INSERT INTO platform.schema_migrations (version) VALUES ('0003');

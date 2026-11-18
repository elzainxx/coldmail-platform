-- platform_0009_suppressions_syntax_rule.down.sql — undo platform_0009: the suppressions CHECK is exactly the one platform_0003 declared (norm/v1 as far as SQL can see it, nothing of module 1's syntax layer).
-- Never refuses: the restored CHECK is weaker than the one it replaces, so every row satisfies it. Rehearsal or a failed deploy only (the migrator refuses `down` unless PLATFORM_MIGRATE_ALLOW_DOWN=true).
SET LOCAL ROLE platform_owner;

ALTER TABLE platform.suppressions DROP CONSTRAINT suppressions_email_norm_v1;
ALTER TABLE platform.suppressions ADD CONSTRAINT suppressions_email_norm_v1 CHECK (
    email = btrim(email)
    AND email = lower(email)
    AND email IS NFC NORMALIZED
    AND octet_length(email) <= 254                                    -- BYTES, as the address limit is stated (length() would count a two-byte letter once)
    AND length(email) - length(replace(email, '@', '')) = 1          -- exactly one @
    AND email ~ '^[^@[:space:]]+@[a-z0-9]([a-z0-9.-]*[a-z0-9])?$'    -- non-empty local part without space; ASCII domain (punycode), no leading/trailing dot or hyphen
    AND split_part(email, '@', 2) !~ '\.\.'                           -- no empty label
    AND split_part(email, '@', 2) !~ '(^|\.)-|-(\.|$)'                -- no label that starts or ends with a hyphen
);
COMMENT ON TABLE platform.suppressions IS 'Per-user suppressed recipient addresses, in module 1''s frozen norm/v1 form, applied to every future job. Hard bounces are NOT auto-suppressed (locked decision). Rows are never updated; nothing deletes one until a removal flow is designed.';

DELETE FROM platform.schema_migrations WHERE version = '0009';

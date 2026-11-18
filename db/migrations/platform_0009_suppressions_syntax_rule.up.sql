-- platform_0009_suppressions_syntax_rule.up.sql — the suppressions CHECK learns module 1's syntax layer. Phase 3 close-out, item A5 (open decision 9, owner ruling).
--
-- THE RULING. The platform's one address rule is norm/v1 PLUS module 1's syntax layer, exactly as module 1 implements it (internal/norm Validate; control-plane/internal/layers SyntaxLayer): after
-- normalisation the local part is at most 64 bytes, the domain at most 255 bytes and the domain holds a dot. Suppressions use the same Go validator, and platform.suppressions stores what it
-- returns. platform_0003 made the CHECK a backstop for what SQL can see of norm/v1; it said nothing of the syntax layer, so it ACCEPTED an address the validator now refuses (a 65-byte local part,
-- "a@localhost"), and ACCEPTED a domain label with "--" in its third and fourth positions (IDNA's CheckHyphens, which norm/v1 refuses and SQL can express).
--
-- WHAT THIS ADDS (three clauses, one constraint, same name):
--   * octet_length(local part) <= 64                        module 1's local-part limit, in BYTES (octet_length, as the 254-byte address limit already is)
--   * the domain contains a '.'                             a one-label domain is not mailable to module 1's layer
--   * no label whose third and fourth bytes are "--"        unless the label starts with "xn--" (a punycode label is decoded first by IDNA, and SQL cannot decode it)
-- The domain limit of 255 bytes is not a clause: the whole address is already limited to 254 bytes, so no domain can reach it; the rule is written in the validator and holds here by that bound.
--
-- WHAT STAYS DIFFERENT, and why it is safe (the harness test TestNormAgreement_Database pins every vector by name):
--   * STRICTER than the validator, deliberately and only stricter: a NUL cannot be stored in text at all; a local part holding white space is refused (an address with a space in it is not mailable and
--     must not be stored with a line feed in it); a domain with an empty label or a leading dot is refused; an address over 254 bytes is refused. Each is a refusal of something the validator
--     accepts, so the table never holds an address the validator would not have produced.
--   * WIDER than the validator, in exactly one place: a label that starts with "xn--" whose punycode is not valid (xn--a), or whose decoded form breaks a rule. SQL cannot decode punycode. The
--     table accepts such a label if it is written to it directly; the platform's writer calls the validator first, which refuses it, and the harness lists the vectors.
--
-- A row that exists when this runs is judged by the new rule (ADD CONSTRAINT validates every row): the table has no writer before Phase 5, so no row can violate it; if one does, the migration
-- fails and says which constraint, and nothing is half applied (the migrator runs each migration in one transaction).
--
-- Created AS platform_owner (see platform_0002), which owns platform.suppressions.

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
    -- module 1's syntax layer (platform_0009)
    AND octet_length(split_part(email, '@', 1)) <= 64                 -- the local part, in bytes
    AND position('.' in split_part(email, '@', 2)) > 0                -- a domain with at least one dot
    -- IDNA CheckHyphens (platform_0009): no label has "--" as its third and fourth bytes, except a punycode label ("xn--")
    AND split_part(email, '@', 2) !~ '(^|\.)(?!xn--)[a-z0-9-]{2}--'
);
COMMENT ON CONSTRAINT suppressions_email_norm_v1 ON platform.suppressions IS 'norm/v1 as far as SQL can see it, plus module 1''s syntax layer (local part <= 64 bytes, a dot in the domain) and IDNA''s hyphen rule (platform_0009). STRICTER than internal/norm Validate on NUL, white space in the local part, an empty label or leading dot and an address over 254 bytes; WIDER only on invalid punycode, which SQL cannot decode (the writer validates first). TestNormAgreement_Database pins every difference by name.';
COMMENT ON TABLE platform.suppressions IS 'Per-user suppressed recipient addresses, in the platform''s one address form (internal/norm Validate: module 1''s norm/v1, then its syntax layer), applied to every future job. Hard bounces are NOT auto-suppressed (locked decision). Rows are never updated; nothing deletes one until a removal flow is designed.';

INSERT INTO platform.schema_migrations (version) VALUES ('0009');

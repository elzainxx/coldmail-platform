-- public-schema-listing.sql — one pipe-separated row per object in schema public: kind, name, parent table (indexes and owned sequences), owned-by-an-extension (t/f).
-- Run with:  psql -X -At -F '|' -v ON_ERROR_STOP=1 -f scripts/public-schema-listing.sql <dsn>      and feed the output to scripts/check-public-schema.sh --listing -
-- Covers: tables (plain and partitioned, partitions included), views, materialized views, sequences, foreign tables, indexes, composite types, enum/domain/range/base types
-- (not array types, not the row type of a table), functions, procedures, aggregates. It does NOT cover: privileges, default privileges, event triggers, operators, casts, text-search
-- objects, collations, or anything outside schema public.

SELECT CASE c.relkind
           WHEN 'r' THEN 'table' WHEN 'p' THEN 'table' WHEN 'v' THEN 'view' WHEN 'm' THEN 'matview' WHEN 'S' THEN 'sequence'
           WHEN 'f' THEN 'foreign-table' WHEN 'i' THEN 'index' WHEN 'I' THEN 'index' WHEN 'c' THEN 'composite-type'
           ELSE 'relkind-' || c.relkind::text
       END,
       c.relname,
       COALESCE(
           (SELECT t.relname FROM pg_index i JOIN pg_class t ON t.oid = i.indrelid WHERE i.indexrelid = c.oid),
           (SELECT t.relname FROM pg_depend d JOIN pg_class t ON t.oid = d.refobjid
             WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.refclassid = 'pg_class'::regclass AND d.deptype IN ('a', 'i') LIMIT 1),
           ''),
       EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
FROM pg_class c
WHERE c.relnamespace = 'public'::regnamespace AND c.relkind <> 't'
UNION ALL
SELECT 'type', t.typname, '',
       EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e')
FROM pg_type t
WHERE t.typnamespace = 'public'::regnamespace AND t.typtype IN ('e', 'd', 'r', 'm', 'b') AND t.typcategory <> 'A'
UNION ALL
SELECT CASE p.prokind WHEN 'f' THEN 'function' WHEN 'p' THEN 'procedure' WHEN 'a' THEN 'aggregate' WHEN 'w' THEN 'window-function' ELSE 'prokind-' || p.prokind::text END,
       p.proname, '',
       EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
FROM pg_proc p
WHERE p.pronamespace = 'public'::regnamespace
ORDER BY 1, 2;

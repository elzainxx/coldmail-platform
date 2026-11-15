//go:build harness

package harness

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The schema locks: catalog tests for the one-way doors of Docs/phase-1-spec.md ("Locked here", items 1-3 and the money rule). They look at schema platform and the two platform tables in public
// (public.users, public.job_items), on a freshly migrated scratch database and on the live one. Each rule has a checker that RETURNS its violations and a paired control: a scratch table that
// breaks the rule, created inside a transaction that is rolled back, must be reported by name. (Module-owned tables in other schemas are out of scope: they cascade from public.users by design, F22.)

type shapeLockViolation struct{ Rule, Msg string }

const (
	shapeRuleMoney   = "credits-are-integers"
	shapeRuleTime    = "instants-are-timestamptz"
	shapeRulePK      = "primary-key-shapes"
	shapeRuleFK      = "no-cascade-into-users"
	shapeRuleComment = "tables-have-comments"
	shapeRuleFloor   = "instrument-floor"
)

// shapePKException is a primary key that is neither a uuid nor a bigint identity, and why it is allowed. A test fails on an exception that no longer matches its table or that the generic rule
// already allows, so the list cannot rot into a blanket permission.
type shapePKException struct {
	Cols []string // "name:type", in key order
	Why  string
}

var shapePKExceptions = map[string]shapePKException{
	"platform.schema_migrations": {[]string{"version:text"}, "the migration tracking table is keyed by the version string ('0000', '0001', ...), the value the migrator compares with the files on disk"},
	"platform.modules":           {[]string{"id:text"}, "a lookup table: the three modules are keyed by their stable short id (m1, m2, m3), which every other table and the module code refer to"},
	"platform.unit_costs":        {[]string{"unit:text"}, "a lookup table: the five billable units are keyed by their name (m1_own, m2_accepted, ...), a code constant that the price snapshot and the ledger carry"},
	"platform.balances":          {[]string{"user_id:uuid", "module_id:text"}, "one row per user per metered module: a composite natural key (user_id, module_id) with no surrogate, so there can never be a second balance for the same pair"},
	"platform.job_unit_prices":   {[]string{"job_id:uuid", "unit:text"}, "one frozen price per unit per job: the composite key (job_id, unit) is what the ledger's frozen-price foreign key points at"},
	"platform.suppressions":      {[]string{"user_id:uuid", "email:text"}, "one row per suppressed address per user: the composite key (user_id, email) is what makes suppressing the same address twice impossible"},
}

const shapeLocksScope = `(n.nspname = 'platform' OR (n.nspname = 'public' AND c.relname IN ('users', 'job_items')))`

var (
	shapeMoneyTypeRe = regexp.MustCompile(`^(numeric|real|double precision|money)(\([0-9, ]+\))?(\[\])*$`)
	shapeTimeTypeRe  = regexp.MustCompile(`^timestamp(\([0-9]+\))? without time zone(\[\])*$`)
)

type shapePKCol struct{ Name, Type, Identity string }

func shapePKText(cols []shapePKCol) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = c.Name + " " + c.Type
	}
	return strings.Join(parts, ", ")
}

// shapePKAllowedGenerically is the rule: one uuid column, or one bigint identity column (GENERATED ALWAYS).
func shapePKAllowedGenerically(cols []shapePKCol) bool {
	if len(cols) != 1 {
		return false
	}
	c := cols[0]
	return c.Type == "uuid" || (c.Type == "bigint" && c.Identity == "a")
}

var shapeFKActions = map[string]string{"a": "NO ACTION", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT"}

// shapeLocksCheck returns every violation of the five locks, each tagged with its rule, plus a floor violation when an instrument looked at too little. It never calls t.Error.
//
// What it covers: every column of every table, view, materialized view and foreign table in schema platform and of public.users / public.job_items (money types, with a domain looked through to its
// base type, and arrays of them; timestamp without time zone); every base table's primary key; every foreign key FROM schema platform TO public.users (its ON DELETE and ON UPDATE action); every
// base table in schema platform for a non-blank COMMENT. What it does NOT cover: money or time types in function arguments and return values (the schema has no function that takes one),
// views' comments, foreign keys from module schemas (they cascade by design), and columns of the right type holding the wrong unit.
func shapeLocksCheck(ctx context.Context, db shapeDB, exceptions map[string]shapePKException) ([]shapeLockViolation, error) {
	var v []shapeLockViolation
	add := func(rule, format string, a ...any) {
		v = append(v, shapeLockViolation{rule, fmt.Sprintf(format, a...)})
	}

	// 1 and 2: column types
	rows, err := db.Query(ctx, `
		SELECT n.nspname || '.' || c.relname, a.attname::text, format_type(a.atttypid, a.atttypmod), format_type(bt.oid, CASE WHEN t.typtype = 'd' THEN t.typtypmod ELSE a.atttypmod END)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		JOIN pg_type t ON t.oid = a.atttypid
		JOIN pg_type bt ON bt.oid = CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE t.oid END
		WHERE `+shapeLocksScope+` AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		ORDER BY 1, a.attnum`)
	if err != nil {
		return nil, err
	}
	columns := 0
	for rows.Next() {
		var tbl, col, declared, base string
		if err := rows.Scan(&tbl, &col, &declared, &base); err != nil {
			rows.Close()
			return nil, err
		}
		columns++
		shown := declared
		if declared != base {
			shown = declared + " (a domain over " + base + ")"
		}
		if shapeMoneyTypeRe.MatchString(base) {
			add(shapeRuleMoney, "%s.%s: type %s is not allowed (credits are integers: bigint amounts and integer unit costs; no numeric, real, double precision or money)", tbl, col, shown)
		}
		if shapeTimeTypeRe.MatchString(base) {
			add(shapeRuleTime, "%s.%s: type %s is not allowed (every instant is timestamptz, stored UTC)", tbl, col, shown)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if columns < 30 {
		add(shapeRuleFloor, "looked at %d column(s) in schema platform and public.users/job_items; the floor is 30: the instrument matched too little", columns)
	}

	// 3: primary keys
	rows, err = db.Query(ctx, `
		SELECT n.nspname || '.' || c.relname, COALESCE(a.attname::text, ''), COALESCE(format_type(a.atttypid, a.atttypmod), ''), COALESCE(a.attidentity::text, '')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_constraint con ON con.conrelid = c.oid AND con.contype = 'p'
		LEFT JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
		LEFT JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		WHERE `+shapeLocksScope+` AND c.relkind IN ('r', 'p')
		ORDER BY 1, k.ord`)
	if err != nil {
		return nil, err
	}
	pks := map[string][]shapePKCol{}
	for rows.Next() {
		var tbl string
		var c shapePKCol
		if err := rows.Scan(&tbl, &c.Name, &c.Type, &c.Identity); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := pks[tbl]; !ok {
			pks[tbl] = nil
		}
		if c.Name != "" {
			pks[tbl] = append(pks[tbl], c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(pks) < 5 {
		add(shapeRuleFloor, "found %d table(s) for the primary-key rule; the floor is 5: the instrument matched too little", len(pks))
	}
	tables := make([]string, 0, len(pks))
	for tbl := range pks {
		tables = append(tables, tbl)
	}
	sort.Strings(tables)
	for _, tbl := range tables {
		cols := pks[tbl]
		generic := shapePKAllowedGenerically(cols)
		exc, hasExc := exceptions[tbl]
		switch {
		case len(cols) == 0:
			add(shapeRulePK, "%s: has no primary key", tbl)
		case generic && hasExc:
			add(shapeRulePK, "%s: needless primary-key exception (its key (%s) already satisfies the rule: remove the exception)", tbl, shapePKText(cols))
		case generic:
		case hasExc:
			got := make([]string, len(cols))
			for i, c := range cols {
				got[i] = c.Name + ":" + c.Type
			}
			if strings.Join(got, ",") != strings.Join(exc.Cols, ",") {
				add(shapeRulePK, "%s: primary key (%s) differs from its declared exception (%s)", tbl, strings.Join(got, ", "), strings.Join(exc.Cols, ", "))
			}
		default:
			add(shapeRulePK, "%s: primary key (%s) is not a uuid column, a bigint identity column (GENERATED ALWAYS) or a declared exception", tbl, shapePKText(cols))
		}
	}
	excTables := make([]string, 0, len(exceptions))
	for tbl := range exceptions {
		excTables = append(excTables, tbl)
	}
	sort.Strings(excTables)
	for _, tbl := range excTables {
		if _, ok := pks[tbl]; !ok {
			add(shapeRulePK, "%s: has a primary-key exception but the table does not exist (the exception is stale)", tbl)
		}
	}

	// 4: foreign keys from schema platform to public.users
	rows, err = db.Query(ctx, `
		SELECT cn.nspname || '.' || cc.relname, con.conname::text, con.confdeltype::text, con.confupdtype::text
		FROM pg_constraint con
		JOIN pg_class cc ON cc.oid = con.conrelid
		JOIN pg_namespace cn ON cn.oid = cc.relnamespace
		WHERE con.contype = 'f' AND cn.nspname = 'platform' AND con.confrelid = to_regclass('public.users')
		ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	fks := 0
	for rows.Next() {
		var tbl, name, del, upd string
		if err := rows.Scan(&tbl, &name, &del, &upd); err != nil {
			rows.Close()
			return nil, err
		}
		fks++
		if (del != "a" && del != "r") || (upd != "a" && upd != "r") {
			add(shapeRuleFK, "%s: foreign key %s to public.users is ON DELETE %s ON UPDATE %s (must be RESTRICT or NO ACTION: nothing cascade-deletes money, history or audit)", tbl, name, shapeFKActions[del], shapeFKActions[upd])
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if fks < 5 {
		add(shapeRuleFloor, "found %d foreign key(s) from schema platform to public.users; the floor is 5: the instrument matched too little", fks)
	}

	// 5: a COMMENT on every table in schema platform
	rows, err = db.Query(ctx, `
		SELECT c.relname::text, btrim(COALESCE(obj_description(c.oid, 'pg_class'), ''))
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'platform' AND c.relkind IN ('r', 'p')
		ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	platformTables := 0
	for rows.Next() {
		var name, comment string
		if err := rows.Scan(&name, &comment); err != nil {
			rows.Close()
			return nil, err
		}
		platformTables++
		if comment == "" {
			add(shapeRuleComment, "platform.%s: the table has no COMMENT (every table in schema platform says what it is for)", name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if platformTables < 5 {
		add(shapeRuleFloor, "found %d table(s) in schema platform for the comment rule; the floor is 5: the instrument matched too little", platformTables)
	}
	return v, nil
}

// shapeLockMsgs returns the messages of the given rules.
func shapeLockMsgs(v []shapeLockViolation, rules ...string) []string {
	var out []string
	for _, x := range v {
		for _, r := range rules {
			if x.Rule == r {
				out = append(out, x.Rule+": "+x.Msg)
			}
		}
	}
	return out
}

func shapeCopyExceptions() map[string]shapePKException {
	out := map[string]shapePKException{}
	for k, v := range shapePKExceptions {
		out[k] = v
	}
	return out
}

// shapeLockControl runs the checker on a fresh migrated scratch database and requires the rule (and the floors) to be clean there (a failure is recorded with t.Errorf, not t.Fatalf, so that
// the control below still runs and shows the rule can fail even while the real database is dirty), then applies `setup` inside a transaction that is rolled back and returns what the checker says
// about that broken database.
func shapeLockControl(t *testing.T, rule string, setup []string) []string {
	t.Helper()
	_, admin := migratedScratch(t)
	ctx := testCtx(t)
	v, err := shapeLocksCheck(ctx, admin, shapePKExceptions)
	if err != nil {
		t.Fatalf("the checker could not run on the freshly migrated scratch database: %v", err)
	}
	if clean := shapeLockMsgs(v, rule, shapeRuleFloor); len(clean) != 0 {
		t.Errorf("a freshly migrated scratch database breaks the lock %q: %d violation(s):\n  %s", rule, len(clean), strings.Join(clean, "\n  "))
	}
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, s := range setup {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	v, err = shapeLocksCheck(ctx, tx, shapePKExceptions)
	if err != nil {
		t.Fatalf("the checker could not run on the broken database: %v", err)
	}
	return shapeLockMsgs(v, rule)
}

// TestSchemaLocks_CreditsAreIntegers: no column of type numeric, real, double precision or money (a domain over one, or an array of one, included) in schema platform or on public.users/job_items;
// the message names the column. Fails when a migration adds one. Control: a scratch table with each of them is reported column by column, and its bigint and integer columns are not.
func TestSchemaLocks_CreditsAreIntegers(t *testing.T) {
	got := shapeLockControl(t, shapeRuleMoney, []string{
		"CREATE DOMAIN platform.zz_money_dom AS numeric(10,2)",
		`CREATE TABLE platform.zz_lock_money (id uuid PRIMARY KEY, c_numeric numeric(12,2), c_plain numeric, c_real real, c_double double precision, c_money money,
			c_array numeric[], c_domain platform.zz_money_dom, c_ok_bigint bigint, c_ok_int integer)`,
		"CREATE VIEW platform.zz_lock_money_view AS SELECT 1.5::numeric AS v",
	})
	for _, col := range []string{"c_numeric", "c_plain", "c_real", "c_double", "c_money", "c_array", "c_domain"} {
		shapeRequireReported(t, got, nil, "platform.zz_lock_money."+col, "is not allowed", "credits are integers")
	}
	shapeRequireReported(t, got, nil, "platform.zz_lock_money_view.v", "numeric")
	shapeRequireReported(t, got, nil, "platform.zz_lock_money.c_domain", "a domain over numeric(10,2)")
	for _, ok := range []string{"c_ok_bigint:", "c_ok_int:", "id:"} {
		if shapeHas(got, "zz_lock_money."+ok) {
			t.Fatalf("the checker reported a bigint/integer/uuid column (%s): it flags more than money types:\n  %s", ok, strings.Join(got, "\n  "))
		}
	}
}

// TestSchemaLocks_InstantsAreTimestamptz: no `timestamp without time zone` column (any precision, arrays included). Fails when a migration adds one. Control: a scratch table with them is reported;
// its timestamptz, time and date columns are not (the lock is about instants).
func TestSchemaLocks_InstantsAreTimestamptz(t *testing.T) {
	got := shapeLockControl(t, shapeRuleTime, []string{
		`CREATE TABLE platform.zz_lock_time (id uuid PRIMARY KEY, c_ts timestamp, c_ts3 timestamp(3), c_arr timestamp[], c_ok timestamptz, c_ok_time time, c_ok_date date)`,
	})
	for _, col := range []string{"c_ts", "c_ts3", "c_arr"} {
		shapeRequireReported(t, got, nil, "platform.zz_lock_time."+col+":", "without time zone", "timestamptz")
	}
	for _, ok := range []string{"c_ok:", "c_ok_time:", "c_ok_date:"} {
		if shapeHas(got, "zz_lock_time."+ok) {
			t.Fatalf("the checker reported %s, which is not a timestamp without time zone:\n  %s", ok, strings.Join(got, "\n  "))
		}
	}
}

// TestSchemaLocks_PrimaryKeyShapes: every table in scope has a primary key that is one uuid column, one bigint GENERATED ALWAYS identity, or a listed exception (shapePKExceptions: the text-keyed
// lookup tables and the composite natural keys, each with its reason). Fails when a migration adds a table with another key, or changes an exception's key. Controls: scratch tables with a serial, a
// text, a plain bigint, a BY DEFAULT identity, a composite and no key are all reported, and a uuid and an ALWAYS identity key are not; a stale exception, a needless exception and an exception that
// no longer matches its table are each reported.
func TestSchemaLocks_PrimaryKeyShapes(t *testing.T) {
	for tbl, e := range shapePKExceptions {
		if len(e.Why) < 40 || len(e.Cols) == 0 {
			t.Fatalf("the primary-key exception for %s has no real reason or no columns (%q): an exception without a reason is a blanket permission", tbl, e.Why)
		}
	}
	got := shapeLockControl(t, shapeRulePK, []string{
		"CREATE TABLE platform.zz_pk_ok_uuid (id uuid PRIMARY KEY)",
		"CREATE TABLE platform.zz_pk_ok_identity (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY)",
		"CREATE TABLE platform.zz_pk_serial (id serial PRIMARY KEY)",
		"CREATE TABLE platform.zz_pk_text (id text PRIMARY KEY)",
		"CREATE TABLE platform.zz_pk_none (a int)",
		"CREATE TABLE platform.zz_pk_bigint_plain (id bigint PRIMARY KEY)",
		"CREATE TABLE platform.zz_pk_identity_default (id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY)",
		"CREATE TABLE platform.zz_pk_composite (a uuid, b uuid, PRIMARY KEY (a, b))",
	})
	shapeRequireReported(t, got, nil, "platform.zz_pk_serial", "(id integer)", "not a uuid column")
	shapeRequireReported(t, got, nil, "platform.zz_pk_text", "(id text)", "not a uuid column")
	shapeRequireReported(t, got, nil, "platform.zz_pk_none", "has no primary key")
	shapeRequireReported(t, got, nil, "platform.zz_pk_bigint_plain", "(id bigint)", "not a uuid column")
	shapeRequireReported(t, got, nil, "platform.zz_pk_identity_default", "(id bigint)", "GENERATED ALWAYS")
	shapeRequireReported(t, got, nil, "platform.zz_pk_composite", "(a uuid, b uuid)")
	if shapeHas(got, "zz_pk_ok") {
		t.Fatalf("the checker reported a uuid or ALWAYS-identity key, which the rule allows:\n  %s", strings.Join(got, "\n  "))
	}

	// the exception list itself is judged: stale, needless and mismatching entries are reported
	_, admin := migratedScratch(t)
	ctx := testCtx(t)
	stale := shapeCopyExceptions()
	stale["platform.zz_no_such_table"] = shapePKException{[]string{"id:text"}, "an exception for a table that was dropped"}
	v, err := shapeLocksCheck(ctx, admin, stale)
	shapeRequireReported(t, shapeLockMsgs(v, shapeRulePK), err, "platform.zz_no_such_table", "does not exist", "stale")

	needless := shapeCopyExceptions()
	needless["platform.jobs"] = shapePKException{[]string{"id:uuid"}, "an exception for a table whose key already satisfies the rule"}
	v, err = shapeLocksCheck(ctx, admin, needless)
	shapeRequireReported(t, shapeLockMsgs(v, shapeRulePK), err, "platform.jobs", "needless primary-key exception")

	changed := shapeCopyExceptions()
	changed["platform.modules"] = shapePKException{[]string{"id:uuid"}, "an exception that no longer matches the table's key"}
	v, err = shapeLocksCheck(ctx, admin, changed)
	shapeRequireReported(t, shapeLockMsgs(v, shapeRulePK), err, "platform.modules", "differs from its declared exception", "id:text")
}

// TestSchemaLocks_NoCascadeIntoUsers: every foreign key from a table in schema platform to public.users is ON DELETE and ON UPDATE RESTRICT or NO ACTION, never CASCADE, SET NULL or SET DEFAULT:
// nothing cascade-deletes money, history or audit. Fails when a migration adds one. Controls: scratch tables with each forbidden action are reported by table and action; RESTRICT, NO ACTION
// and the default are not; and a MODULE table that cascades from public.users (the modules' own foreign keys do, finding F22) is out of scope and not reported.
func TestSchemaLocks_NoCascadeIntoUsers(t *testing.T) {
	got := shapeLockControl(t, shapeRuleFK, []string{
		"CREATE TABLE platform.zz_fk_cascade (id uuid PRIMARY KEY, user_id uuid REFERENCES public.users (id) ON DELETE CASCADE)",
		"CREATE TABLE platform.zz_fk_setnull (id uuid PRIMARY KEY, user_id uuid REFERENCES public.users (id) ON DELETE SET NULL)",
		"CREATE TABLE platform.zz_fk_setdefault (id uuid PRIMARY KEY, user_id uuid DEFAULT '00000000-0000-0000-0000-000000000000' REFERENCES public.users (id) ON DELETE SET DEFAULT)",
		"CREATE TABLE platform.zz_fk_updcascade (id uuid PRIMARY KEY, user_id uuid REFERENCES public.users (id) ON UPDATE CASCADE)",
		"CREATE TABLE platform.zz_fk_ok_restrict (id uuid PRIMARY KEY, user_id uuid REFERENCES public.users (id) ON DELETE RESTRICT ON UPDATE RESTRICT)",
		"CREATE TABLE platform.zz_fk_ok_noaction (id uuid PRIMARY KEY, user_id uuid REFERENCES public.users (id) ON DELETE NO ACTION)",
		"CREATE TABLE platform.zz_fk_ok_default (id uuid PRIMARY KEY, user_id uuid REFERENCES public.users (id))",
		"CREATE SCHEMA zz_module",
		"CREATE TABLE zz_module.jobs (id uuid PRIMARY KEY, user_id uuid REFERENCES public.users (id) ON DELETE CASCADE)",
	})
	shapeRequireReported(t, got, nil, "platform.zz_fk_cascade", "ON DELETE CASCADE", "RESTRICT or NO ACTION")
	shapeRequireReported(t, got, nil, "platform.zz_fk_setnull", "ON DELETE SET NULL")
	shapeRequireReported(t, got, nil, "platform.zz_fk_setdefault", "ON DELETE SET DEFAULT")
	shapeRequireReported(t, got, nil, "platform.zz_fk_updcascade", "ON UPDATE CASCADE")
	if shapeHas(got, "zz_fk_ok") || shapeHas(got, "zz_module") {
		t.Fatalf("the checker reported a RESTRICT / NO ACTION key or a module-schema table, which are out of the rule:\n  %s", strings.Join(got, "\n  "))
	}
}

// TestSchemaLocks_EveryTableHasAComment: every base table in schema platform has a non-blank COMMENT. Fails when a migration creates a table without one. Control: scratch tables with no comment
// and with a blank comment are reported by name; one with a real comment is not.
func TestSchemaLocks_EveryTableHasAComment(t *testing.T) {
	got := shapeLockControl(t, shapeRuleComment, []string{
		"CREATE TABLE platform.zz_comment_missing (id uuid PRIMARY KEY)",
		"CREATE TABLE platform.zz_comment_blank (id uuid PRIMARY KEY)",
		"COMMENT ON TABLE platform.zz_comment_blank IS '   '",
		"CREATE TABLE platform.zz_comment_ok (id uuid PRIMARY KEY)",
		"COMMENT ON TABLE platform.zz_comment_ok IS 'a real comment'",
	})
	shapeRequireReported(t, got, nil, "platform.zz_comment_missing", "no COMMENT")
	shapeRequireReported(t, got, nil, "platform.zz_comment_blank", "no COMMENT")
	if shapeHas(got, "zz_comment_ok") {
		t.Fatalf("the checker reported a table that has a comment:\n  %s", strings.Join(got, "\n  "))
	}
}

// TestSchemaLocks_FloorsBite: an instrument that looked at nothing must say so. With schema platform dropped (inside a rolled-back transaction) only public.users and public.job_items are left, and
// the checker reports that every one of its floors was not met; on the real database none is.
func TestSchemaLocks_FloorsBite(t *testing.T) {
	_, admin := migratedScratch(t)
	ctx := testCtx(t)
	v, err := shapeLocksCheck(ctx, admin, shapePKExceptions)
	shapeRequireClean(t, shapeLockMsgs(v, shapeRuleFloor), err, "the control: the migrated scratch database")

	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "DROP SCHEMA platform CASCADE"); err != nil {
		t.Fatalf("drop schema platform: %v", err)
	}
	v, err = shapeLocksCheck(ctx, tx, shapePKExceptions)
	got := shapeLockMsgs(v, shapeRuleFloor)
	shapeRequireReported(t, got, err, "column(s) in schema platform", "the floor is 30")
	shapeRequireReported(t, got, err, "for the primary-key rule", "the floor is 5")
	shapeRequireReported(t, got, err, "foreign key(s) from schema platform to public.users", "the floor is 5")
	shapeRequireReported(t, got, err, "in schema platform for the comment rule", "the floor is 5")
}

// TestSchemaLocks_LiveDatabase runs every lock on the LIVE database the harness stack migrated (read-only).
func TestSchemaLocks_LiveDatabase(t *testing.T) {
	requireLiveMigrated(t)
	v, err := shapeLocksCheck(testCtx(t), liveAdmin(t), shapePKExceptions)
	var all []string
	for _, x := range v {
		all = append(all, x.Rule+": "+x.Msg)
	}
	shapeRequireClean(t, all, err, "the live database")
}

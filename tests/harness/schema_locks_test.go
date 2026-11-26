//go:build harness

package harness

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
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
	"platform.reserves":          {[]string{"reserve_id:bigint"}, "one row per reserve: the key IS the ledger id of the reserve row it describes (a foreign key to platform.ledger written by the ledger trigger), so a second row for one reserve is impossible and no surrogate id can drift from it"},
	"platform.job_results":       {[]string{"job_id:uuid", "item_ref:uuid"}, "one result per job row: the key (job_id, item_ref) IS the row it explains (a composite foreign key to platform.job_rows), so a row has one outcome and a result cannot name a row of another job"},
	"platform.job_transitions":   {[]string{"from_status:text", "to_status:text"}, "one row per legal move of a job: the composite key (from_status, to_status) is what the lifecycle guard looks a move up by, so a move cannot be legal twice or carry two sets of actors"},
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

// ---------------------------------------------------------------------------------------------------------------------
// platform_0004 (the owner's rulings on the Phase 1 open decisions): what it leaves in the catalog. The five locks above are generic
// rules and platform_0004 breaks none of them (it adds no table, no key, no foreign key, no money or time column); these are the
// objects it ADDS or REPLACES, pinned by name so that a later migration that drops, weakens or renames one fails here.
// ---------------------------------------------------------------------------------------------------------------------

const phase2ZeroUUID = "00000000-0000-0000-0000-000000000000"

var phase2Literal = regexp.MustCompile(`'([a-z_0-9]+)'`)

// phase2Rows runs a query and returns every row as strings (a NULL is "<nil>": the queries below COALESCE what may be NULL).
func phase2Rows(ctx context.Context, q querier, sql string, args ...any) ([][]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := make([]string, len(vals))
		for i, x := range vals {
			row[i] = fmt.Sprint(x)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// phase2Literals is the sorted set of quoted words in a deparsed expression: the statuses of an index predicate, the kinds of a CHECK or of a generated column.
func phase2Literals(expr string) []string {
	set := map[string]bool{}
	for _, m := range phase2Literal.FindAllStringSubmatch(expr, -1) {
		set[m[1]] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// phase2TriggerText renders pg_trigger.tgtype (ROW 1, BEFORE 2, INSERT 4, DELETE 8, UPDATE 16, TRUNCATE 32, INSTEAD OF 64) as the sentence a trigger definition would say.
func phase2TriggerText(n int) string {
	timing := "AFTER"
	if n&2 != 0 {
		timing = "BEFORE"
	} else if n&64 != 0 {
		timing = "INSTEAD OF"
	}
	var ev []string
	for _, e := range []struct {
		bit  int
		name string
	}{{4, "INSERT"}, {8, "DELETE"}, {16, "UPDATE"}, {32, "TRUNCATE"}} {
		if n&e.bit != 0 {
			ev = append(ev, e.name)
		}
	}
	each := "FOR EACH STATEMENT"
	if n&1 != 0 {
		each = "FOR EACH ROW"
	}
	return timing + " " + strings.Join(ev, " OR ") + " " + each
}

// phase2CatalogCheck returns every way the catalog differs from what platform_0004 builds, as sentences naming the object; it never calls t.Error. It covers: the slot index
// (unique, over (user_id, module_id), partial over exactly scheduled/running/paused/cancelling, the old index gone); the four system-user CHECKs, ledger_shape_adjustment and the
// vocabulary CHECK (each named, a CHECK, validated, saying what it must); the ledger's module-metering foreign key; the generated column monetary (stored, naming exactly the five monetary
// kinds); the four triggers (timing, events, row-level, WHEN, function, enabled); the functions (owner, return type, a pinned search_path, SECURITY DEFINER where the design needs it) and
// unit_costs_guard calling the admin check; and the view listable_users (a view, its columns, both of its exclusions). It also pins what platform_0005 and platform_0006 add, by the same
// method: the CHECKs of the one-unit charge, the item reference, the per-reserve cap, the end date and the delivery table; the three unique partial indexes of the ledger (one charge per job
// item, one reserve per job, one release per reserve); the six triggers (timing, events, WHEN, enabled; the first to run is the created_at stamp) and their functions (SECURITY DEFINER except the stamp), whose BODIES must still name the constraints
// they raise (a no-op body would keep every other fact). platform_0007's two triggers are pinned the same way: the snapshot price check (BEFORE INSERT on job_unit_prices) and the terminal-state rule
// (BEFORE UPDATE on jobs), both SECURITY INVOKER with a pinned search_path, their bodies naming the constraints they raise and (for the rule) all three terminal statuses.
//
// What it does NOT cover, and says so: whether the triggers BEHAVE (the constraint and ledger tests do that, with a control for each), who may EXECUTE the functions (checkFunctionGrants), the
// view's privileges (checkGrants) and the exact text of an expression (it reads the quoted words and the key tokens, which PostgreSQL deparses the same way on every version it runs on).
func phase2CatalogCheck(ctx context.Context, q querier) ([]string, error) {
	var v []string
	add := func(format string, a ...any) { v = append(v, fmt.Sprintf(format, a...)) }

	// 1. the slot index
	rows, err := phase2Rows(ctx, q, `SELECT c.relname::text FROM pg_class c WHERE c.relnamespace = 'platform'::regnamespace AND c.relkind = 'i' AND c.relname::text = 'jobs_one_executing_per_user_module'`)
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		add("platform.jobs_one_executing_per_user_module: the OLD slot index still exists beside its replacement (platform_0004 drops it)")
	}
	rows, err = phase2Rows(ctx, q, `SELECT i.indisunique::text, COALESCE(pg_get_expr(i.indpred, i.indrelid), ''), pg_get_indexdef(i.indexrelid), (i.indrelid = to_regclass('platform.jobs'))::text
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relnamespace = 'platform'::regnamespace AND c.relname::text = 'jobs_one_active_per_user_module'`)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		add("platform.jobs_one_active_per_user_module: missing (the slot index of platform_0004, widened by platform_0011: a confirmed, scheduled, running, paused, cancelling or completing job holds the slot)")
	} else {
		r := rows[0]
		if r[0] != "true" {
			add("platform.jobs_one_active_per_user_module: not unique (the slot is a UNIQUE partial index)")
		}
		if r[3] != "true" {
			add("platform.jobs_one_active_per_user_module: not an index of platform.jobs")
		}
		if !strings.Contains(r[2], "(user_id, module_id)") {
			add("platform.jobs_one_active_per_user_module: not over (user_id, module_id): %s", r[2])
		}
		if got, want := phase2Literals(r[1]), []string{"cancelling", "completing", "confirmed", "paused", "running", "scheduled"}; strings.Join(got, ",") != strings.Join(want, ",") {
			add("platform.jobs_one_active_per_user_module: its predicate covers the statuses %v, want exactly %v (a scheduled job holds the slot since platform_0004; a confirmed and a completing one since platform_0011)", got, want)
		}
	}

	// 2. the CHECKs
	sys := []string{"user_id", "<>", phase2ZeroUUID}
	for _, c := range []struct {
		rel, name string
		tokens    []string
	}{
		{"platform.balances", "balances_not_the_system_user", sys},
		{"platform.ledger", "ledger_not_the_system_user", sys},
		{"platform.m3_entitlements", "m3_entitlements_not_the_system_user", sys},
		{"platform.suppressions", "suppressions_not_the_system_user", sys},
		{"platform.ledger", "ledger_shape_adjustment", []string{"'adjustment'", "available_delta < 0", "reserved_delta = 0", "job_id IS NULL", "reserve_id IS NULL", "unit_cost IS NULL",
			"acting_admin_id IS NOT NULL", "reason IS NOT NULL", "btrim(reason)"}},
		{"platform.ledger", "ledger_kind_vocabulary", []string{"'grant'", "'reserve'", "'charge'", "'release'", "'adjustment'", "'m3_enable'", "'m3_disable'", "'m3_expire'"}},
		// platform_0005: a charge is one unit of one named job item; the per-reserve cap
		{"platform.ledger", "ledger_item_ref_shape", []string{"'charge'", "item_ref IS NOT NULL", "octet_length(item_ref) <= 200", "item_ref IS NULL"}},
		{"platform.ledger", "ledger_charge_is_one_item", []string{"'charge'", "COALESCE", "units = 1"}},
		{"platform.reserves", "reserves_not_overspent", []string{"(charged + released) <= reserved"}},
		// platform_0006: the end date rides only on an m3_enable row; the delivery table's rules (the convergence rule is the one that stops a false success)
		{"platform.ledger", "ledger_until_at_shape", []string{"until_at IS NULL", "'m3_enable'", "until_at > created_at"}},
		// platform_0008 replaced the platform_0006 rule (m3_sync_converged_only_when_observed) with this one: the observation must also be of THIS version, and a NULL version is refused (IS NOT NULL is stated, so the CHECK cannot evaluate to NULL and pass)
		{"platform.m3_sync", "m3_sync_converged_only_when_observed_at_version", []string{"converged_version < want_version", "observed_version IS NOT NULL", "observed_version = want_version", "NOT want_suspended", "'suspended'", "'active'"}},
		{"platform.m3_sync", "m3_sync_converged_not_ahead", []string{"converged_version <= want_version"}},
		{"platform.m3_sync", "m3_sync_observed_vocabulary", []string{"'unknown'", "'suspended'", "'active'"}},
		// platform_0011: the twelve statuses; what a job that has left previewed must have (and that the fields it names are tested with IS NOT NULL, so the CHECK cannot evaluate to NULL and pass); a start deadline for
		// confirmed and scheduled, none for a confirmed job's start time; where a paused job paused from; the shapes of the confirm key, the fingerprint and the preview
		{"platform.jobs", "jobs_status_vocabulary", []string{"'draft'", "'previewed'", "'confirmed'", "'scheduled'", "'running'", "'paused'", "'cancelling'", "'completing'", "'succeeded'", "'failed'", "'cancelled'", "'expired'"}},
		{"platform.jobs", "jobs_confirmed_shape", []string{"'confirmed'", "'scheduled'", "'running'", "'paused'", "'cancelling'", "'completing'", "preview IS NOT NULL", "preview_fingerprint IS NOT NULL", "confirm_key IS NOT NULL", "confirmed_at IS NOT NULL"}},
		{"platform.jobs", "jobs_start_needs_a_deadline", []string{"'confirmed'", "'scheduled'", "start_deadline IS NOT NULL"}},
		{"platform.jobs", "jobs_confirmed_starts_now", []string{"'confirmed'", "scheduled_start IS NULL"}},
		{"platform.jobs", "jobs_paused_from_shape", []string{"'paused'", "paused_from IS NOT NULL"}},
		{"platform.jobs", "jobs_paused_from_vocabulary", []string{"paused_from IS NULL", "'scheduled'", "'running'"}},
		{"platform.jobs", "jobs_confirm_key_shape", []string{"confirm_key IS NULL", "128", "A-Za-z0-9"}},
		{"platform.jobs", "jobs_fingerprint_shape", []string{"preview_fingerprint IS NULL", "[0-9a-f]{64}"}},
		{"platform.jobs", "jobs_preview_is_an_object", []string{"preview IS NULL", "jsonb_typeof", "'object'"}},
		{"platform.job_events", "job_events_actor_shape", []string{"'user'", "'admin'", "'engine'", "acting_user_id IS NOT NULL", "acting_admin_id IS NULL", "acting_admin_id IS NOT NULL", "acting_user_id IS NULL"}},
		{"platform.job_events", "job_events_transition_shape", []string{"'transition'", "from_status IS NOT NULL", "to_status IS NOT NULL"}},
		{"platform.job_events", "job_events_reason_not_blank", []string{"btrim(reason)"}},
		// platform_0012: the list of a job (five states, the normalised address is NULL exactly for a malformed row, a duplicate names the row it repeats, a malformed or excluded row says why, only a pending row is
		// submitted, the fields are an object, the raw address is bounded) and its results (a charged result names its unit, the vocabularies, nothing blank)
		{"platform.job_rows", "job_rows_state_vocabulary", []string{"'pending'", "'malformed'", "'duplicate'", "'suppressed'", "'excluded'"}},
		{"platform.job_rows", "job_rows_email_norm_shape", []string{"'malformed'", "email_norm IS NULL"}},
		{"platform.job_rows", "job_rows_duplicate_shape", []string{"'duplicate'", "duplicate_of IS NOT NULL"}},
		{"platform.job_rows", "job_rows_reason_shape", []string{"'malformed'", "'excluded'", "reason IS NOT NULL", "btrim(reason)"}},
		{"platform.job_rows", "job_rows_only_pending_is_submitted", []string{"submitted_at IS NULL", "'pending'"}},
		{"platform.job_rows", "job_rows_fields_is_an_object", []string{"jsonb_typeof(fields)", "'object'"}},
		{"platform.job_rows", "job_rows_email_raw_bounded", []string{"octet_length(email_raw)", "4096"}},
		{"platform.job_rows", "job_rows_ordinal_positive", []string{"ordinal >= 1"}},
		{"platform.job_results", "job_results_charged_names_its_unit", []string{"'charge'", "unit IS NOT NULL"}},
		{"platform.job_results", "job_results_decision_vocabulary", []string{"'charge'", "'free'", "'unmapped'"}},
		{"platform.job_results", "job_results_origin_vocabulary", []string{"'module'", "'platform'"}},
		{"platform.job_results", "job_results_state_not_blank", []string{"btrim(module_state)", "btrim(rule)"}},
		{"platform.job_results", "job_results_detail_is_an_object", []string{"jsonb_typeof(detail)", "'object'"}},
		// platform_0013: the reachability flag's source is named when it is stated, and the history says who said so
		{"platform.unit_costs", "unit_costs_reachable_source_not_blank", []string{"reachable_source IS NULL", "btrim(reachable_source)"}},
		{"platform.unit_reachability_history", "unit_reachability_history_source_not_blank", []string{"btrim(source)"}},
		// platform_0014 (owner ruling OD-3): a suppressed row carries the flag, the flag is only on a row that is pending or suppressed, the platform's own suppressed result carries it
		{"platform.job_rows", "job_rows_suppressed_state_has_the_flag", []string{"'suppressed'", "OR suppressed"}},
		{"platform.job_rows", "job_rows_flag_only_on_listable_rows", []string{"NOT suppressed", "'pending'", "'suppressed'"}},
		{"platform.job_results", "job_results_suppressed_platform_outcome", []string{"'platform'", "'suppressed'", "OR suppressed"}},
	} {
		rows, err := phase2Rows(ctx, q, `SELECT c.contype::text, c.convalidated::text, pg_get_constraintdef(c.oid) FROM pg_constraint c WHERE c.conrelid = to_regclass($1::text) AND c.conname::text = $2`, c.rel, c.name)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			add("%s.%s: missing", c.rel, c.name)
			continue
		}
		r := rows[0]
		if r[0] != "c" {
			add("%s.%s: is not a CHECK constraint (type %q)", c.rel, c.name, r[0])
		}
		if r[1] != "true" {
			add("%s.%s: not validated", c.rel, c.name)
		}
		for _, tok := range c.tokens {
			if strings.Contains(r[2], tok) {
				continue
			}
			if tok == phase2ZeroUUID {
				add("%s.%s: the CHECK does not name the all-zero system user: %s", c.rel, c.name, r[2])
			} else {
				add("%s.%s: its definition lacks %q: %s", c.rel, c.name, tok, r[2])
			}
		}
	}
	rows, err = phase2Rows(ctx, q, `SELECT c.contype::text, (c.confrelid = to_regclass('platform.modules'))::text,
		COALESCE((SELECT string_agg(a.attname::text, ',' ORDER BY k.ord) FROM unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum), ''),
		COALESCE((SELECT string_agg(a.attname::text, ',' ORDER BY k.ord) FROM unnest(c.confkey) WITH ORDINALITY AS k(attnum, ord) JOIN pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = k.attnum), '')
		FROM pg_constraint c WHERE c.conrelid = to_regclass('platform.ledger') AND c.conname::text = 'ledger_module_metering_fk'`)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		add("platform.ledger.ledger_module_metering_fk: missing (re-created by platform_0004 over the new generated column)")
	} else if r := rows[0]; r[0] != "f" || r[1] != "true" || r[2] != "module_id,monetary" || r[3] != "id,metered" {
		add("platform.ledger.ledger_module_metering_fk: is %s (%s) -> modules=%s (%s), want a foreign key (module_id,monetary) -> platform.modules (id,metered)", r[0], r[2], r[1], r[3])
	}

	// 3. the generated column
	rows, err = phase2Rows(ctx, q, `SELECT a.attgenerated::text, pg_get_expr(d.adbin, d.adrelid) FROM pg_attribute a JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = to_regclass('platform.ledger') AND a.attname::text = 'monetary' AND NOT a.attisdropped`)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		add("platform.ledger.monetary: missing (a generated column with an expression)")
	} else {
		if rows[0][0] != "s" {
			add("platform.ledger.monetary: not a STORED generated column (attgenerated %q)", rows[0][0])
		}
		if got, want := phase2Literals(rows[0][1]), []string{"adjustment", "charge", "grant", "release", "reserve"}; strings.Join(got, ",") != strings.Join(want, ",") {
			add("platform.ledger.monetary: the generated expression names the kinds %v, want exactly %v (an adjustment is money: the balance trigger and balance_drift read this column)", got, want)
		}
	}

	// 4. the triggers
	for _, tr := range []struct {
		rel, name string
		typ       int
		when      bool
	}{
		{"platform.ledger", "ledger_require_active_admin", 1 + 2 + 4, false},
		{"platform.ledger", "ledger_audit_adjustment", 1 + 4, true},
		{"platform.admin_audit", "admin_audit_require_active_admin", 1 + 2 + 4, false},
		{"platform.m3_entitlements", "m3_entitlements_require_active_admin", 1 + 2 + 4 + 16, false},
		{"platform.unit_costs", "unit_costs_guard", 1 + 2 + 16, false},
		// platform_0005: BEFORE the row is written the reserve must be at the dearest price of a complete snapshot and a release exactly the remainder (each only for its own kind); AFTER it,
		// platform.reserves follows the row
		{"platform.ledger", "ledger_check_reserve", 1 + 2 + 4, true},
		{"platform.ledger", "ledger_check_release", 1 + 2 + 4, true},
		{"platform.ledger", "ledger_apply_to_reserves", 1 + 4, true},
		// platform_0005, the first thing to run on a ledger row (BEFORE INSERT, every kind, no WHEN): created_at is overwritten with the transaction's now(), so no writer can supply the time
		{"platform.ledger", "ledger_stamp_created_at", 1 + 2 + 4, false},
		// platform_0006: the ledger writes the entitlement (AFTER, only for the three entitlement kinds) and the entitlement raises its delivery row (AFTER INSERT OR UPDATE)
		{"platform.ledger", "ledger_apply_to_entitlements", 1 + 4, true},
		{"platform.m3_entitlements", "m3_entitlements_raise_sync", 1 + 4 + 16, false},
		// platform_0007: BEFORE the row is written a snapshot price must equal the table's (every INSERT, no WHEN), and BEFORE a job row is updated a terminal status must not change (every UPDATE, no WHEN: the
		// comparison is in the body, so a column list or a WHEN could not leave a status change through)
		{"platform.job_unit_prices", "job_unit_prices_snapshot_equals_unit_costs", 1 + 2 + 4, false},
		{"platform.jobs", "jobs_terminal_is_final", 1 + 2 + 16, false},
		// platform_0011: BEFORE a job row is written or updated the lifecycle guard decides (every INSERT and UPDATE, no WHEN: the body compares the status itself, so a WHEN could not leave a status change through);
		// AFTER an UPDATE OF status that changed it, the event is written (a WHEN: only a changed status is a move); BEFORE an event row is written its actor is checked. (The append-only triggers of the two new
		// tables call platform.reject_mutation, which this list, that matches a trigger to its function by name, cannot hold: they are proven by behaviour in TestJobLifecycle_*.)
		{"platform.jobs", "jobs_transition_guard", 1 + 2 + 4 + 16, false},
		{"platform.jobs", "jobs_record_transition", 1 + 16, true},
		{"platform.job_events", "job_events_check_actor", 1 + 2 + 4, false},
		// platform_0012: BEFORE a row of a job's list is written, changed or removed the job must still be draft or previewed (every row event, no WHEN: the body looks at what changed), and BEFORE a result is
		// written the job must have left previewed
		{"platform.job_rows", "job_rows_frozen", 1 + 2 + 4 + 8 + 16, false},
		{"platform.job_results", "job_results_after_confirm", 1 + 2 + 4, false},
		// platform_0013: BEFORE a snapshot row is written its unit must be reachable; AFTER a change of the flag the history is written (a WHEN: only a changed flag)
		{"platform.job_unit_prices", "job_unit_prices_only_reachable", 1 + 2 + 4, false},
		{"platform.unit_costs", "unit_costs_record_reachability", 1 + 16, true},
	} {
		rows, err := phase2Rows(ctx, q, `SELECT t.tgtype::int::text, t.tgenabled::text, (t.tgqual IS NOT NULL)::text, p.proname::text FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
			WHERE NOT t.tgisinternal AND t.tgrelid = to_regclass($1::text) AND t.tgname::text = $2`, tr.rel, tr.name)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			add("%s trigger %s: missing", tr.rel, tr.name)
			continue
		}
		r := rows[0]
		if r[0] != strconv.Itoa(tr.typ) {
			got, _ := strconv.Atoi(r[0])
			add("%s trigger %s: fires as %s, want %s", tr.rel, tr.name, phase2TriggerText(got), phase2TriggerText(tr.typ))
		}
		if r[1] != "O" {
			add("%s trigger %s: not enabled (state %q, want O)", tr.rel, tr.name, r[1])
		}
		if (r[2] == "true") != tr.when {
			add("%s trigger %s: WHEN clause present = %s, want %v", tr.rel, tr.name, r[2], tr.when)
		}
		if r[3] != tr.name {
			add("%s trigger %s: calls %s(), want %s()", tr.rel, tr.name, r[3], tr.name)
		}
	}

	// 5. the functions
	for _, f := range []struct {
		name, args, ret string
		definer         bool // must be SECURITY DEFINER (it reads or writes what the caller's role cannot)
	}{
		{"require_active_admin", "uuid, text", "void", true},
		{"ledger_audit_adjustment", "", "trigger", true},
		{"ledger_require_active_admin", "", "trigger", false},
		{"admin_audit_require_active_admin", "", "trigger", false},
		{"m3_entitlements_require_active_admin", "", "trigger", false},
		// platform_0005 and platform_0006: each reads or writes what the application roles cannot (the price snapshot, platform.reserves, platform.m3_entitlements, platform.m3_sync)
		{"ledger_check_reserve", "", "trigger", true},
		{"ledger_check_release", "", "trigger", true},
		{"ledger_apply_to_reserves", "", "trigger", true},
		{"ledger_stamp_created_at", "", "trigger", false}, // SECURITY INVOKER: it reads and writes nothing but the row it is given
		{"ledger_apply_to_entitlements", "", "trigger", true},
		{"m3_entitlements_raise_sync", "", "trigger", true},
		// platform_0007: SECURITY INVOKER both (the price check reads platform.unit_costs, which platform_app may read; the terminal-state rule reads only the row it is given), with a pinned search_path
		{"job_unit_prices_snapshot_equals_unit_costs", "", "trigger", false},
		{"jobs_terminal_is_final", "", "trigger", false},
		// platform_0011: the guard and the event writer read what the caller may read (platform.job_transitions, platform.modules) and write what the caller may write (platform.job_events), so both are SECURITY
		// INVOKER; the actor check reads platform.admins FOR SHARE through require_active_admin and platform.jobs, which the roles cannot lock, so it is a definer; platform.job_transition is the store's
		// move, SECURITY INVOKER (it must not lend the owner's privileges to whoever may call it) and it returns the updated job row
		{"jobs_transition_guard", "", "trigger", false},
		{"jobs_record_transition", "", "trigger", false},
		{"job_events_check_actor", "", "trigger", true},
		{"job_transition", "uuid, text, text, uuid, text", "platform.jobs", false},
		// platform_0012 and platform_0013: the two guards read platform.jobs and the snapshot check platform.unit_costs, which platform_app may read, so all SECURITY INVOKER; the history writer runs inside
		// set_unit_reachable and writes the table the roles cannot; set_unit_reachable is the one writer of the flag and a SECURITY DEFINER (platform_app holds no UPDATE on those columns)
		{"job_rows_frozen", "", "trigger", false},
		{"job_results_after_confirm", "", "trigger", false},
		{"job_unit_prices_only_reachable", "", "trigger", false},
		{"unit_costs_record_reachability", "", "trigger", false},
		{"set_unit_reachable", "text, boolean, text", "boolean", true},
	} {
		rows, err := phase2Rows(ctx, q, `SELECT p.prosecdef::text, (EXISTS (SELECT 1 FROM unnest(COALESCE(p.proconfig, ARRAY[]::text[])) cfg WHERE cfg = 'search_path=pg_catalog, pg_temp'))::text,
				pg_get_userbyid(p.proowner), p.prorettype::regtype::text
			FROM pg_proc p WHERE p.pronamespace = 'platform'::regnamespace AND p.proname::text = $1 AND oidvectortypes(p.proargtypes) = $2`, f.name, f.args)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			add("platform.%s(%s): missing", f.name, f.args)
			continue
		}
		r := rows[0]
		if r[2] != "platform_owner" {
			add("platform.%s(%s): owned by %s, want platform_owner", f.name, f.args, r[2])
		}
		if strings.TrimPrefix(r[3], "platform.") != strings.TrimPrefix(f.ret, "platform.") { // a composite type of schema platform prints qualified or not by the session's search_path
			add("platform.%s(%s): returns %s, want %s", f.name, f.args, r[3], f.ret)
		}
		if r[1] != "true" {
			add("platform.%s(%s): does not pin search_path = pg_catalog, pg_temp", f.name, f.args)
		}
		if f.definer && r[0] != "true" {
			add("platform.%s(%s): is not SECURITY DEFINER (the application roles cannot do what it does)", f.name, f.args)
		}
	}
	rows, err = phase2Rows(ctx, q, `SELECT (p.prosrc LIKE '%require_active_admin%')::text FROM pg_proc p WHERE p.pronamespace = 'platform'::regnamespace AND p.proname::text = 'unit_costs_guard'`)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		add("platform.unit_costs_guard(): missing")
	} else if rows[0][0] != "true" {
		add("platform.unit_costs_guard(): does not call require_active_admin (a disabled admin could change a price)")
	}
	// platform_0011: the three functions that run for the CALLER must stay SECURITY INVOKER: a definer job_transition would lend the owner's UPDATE on platform.jobs to whoever may call it, and a definer guard
	// or event writer would write as the owner and hide who did it
	rows, err = phase2Rows(ctx, q, `SELECT p.proname::text FROM pg_proc p WHERE p.pronamespace = 'platform'::regnamespace AND p.prosecdef
		AND p.proname::text IN ('job_transition', 'jobs_transition_guard', 'jobs_record_transition', 'job_rows_frozen', 'job_results_after_confirm', 'job_unit_prices_only_reachable', 'unit_costs_record_reachability') ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		add("platform.%s(): is SECURITY DEFINER, want SECURITY INVOKER (it must run with the caller's privileges)", r[0])
	}

	// 5b. what the new trigger functions DO (the quoted names are the constraint names a refusal carries): a body replaced by a no-op keeps the trigger, the owner and the search_path, and
	// would pass every check above
	for _, b := range []struct {
		fn     string
		tokens []string
	}{
		{"ledger_check_reserve", []string{"reserve_at_the_dearest_price", "reserve_needs_the_complete_snapshot", "job_unit_prices", "unit_costs", "reachable"}},
		{"ledger_check_release", []string{"release_is_the_remainder", "platform.reserves"}},
		{"ledger_apply_to_reserves", []string{"platform.reserves", "charged", "released", "reserve_row_exists"}},
		{"ledger_apply_to_entitlements", []string{"m3_expire_is_due", "platform.m3_entitlements", "m3_enable", "m3_disable", "m3_expire"}},
		{"m3_entitlements_raise_sync", []string{"platform.m3_sync", "want_version", "ON CONFLICT"}},
		{"ledger_stamp_created_at", []string{"NEW.created_at", "now()"}},
		{"job_unit_prices_snapshot_equals_unit_costs", []string{"snapshot_equals_unit_costs", "platform.unit_costs", "IS DISTINCT FROM"}},
		{"jobs_terminal_is_final", []string{"jobs_terminal_is_final", "'succeeded'", "'failed'", "'cancelled'", "'expired'", "IS DISTINCT FROM"}},
		// platform_0011: the guard names every refusal it raises (the constraint names a writer and a test see), reads the lifecycle table and the module's capabilities, and a no-op body would keep the triggers
		{"jobs_transition_guard", []string{"job_born_draft", "job_transition_illegal", "job_transition_needs_an_actor", "job_transition_actor_not_allowed", "job_transition_by_another_user", "job_module_cannot_pause",
			"job_module_cannot_cancel", "job_resume_to_where_it_paused_from", "job_unsubmitted_pause_never_cancelling", "job_unsubmitted_pause_never_completing", "job_submitted_pause_cancels_through_the_engine", "job_started_early", "job_expired_early", "job_cancelled_cannot_succeed", "job_cancelled_needs_a_cancel",
			"platform.job_transitions", "platform.modules", "platform.actor_kind", "platform.actor_id", "platform.reason", "IS DISTINCT FROM"}},
		{"jobs_record_transition", []string{"platform.job_events", "platform.actor_kind", "platform.actor_id", "platform.reason", "'transition'"}},
		{"job_events_check_actor", []string{"require_active_admin", "job_event_actor_is_the_owner", "platform.jobs"}},
		{"job_transition", []string{"platform.actor_kind", "platform.actor_id", "platform.reason", "UPDATE platform.jobs", "RETURNING"}},
		{"job_rows_frozen", []string{"job_rows_frozen", "platform.jobs", "'draft'", "'previewed'", "submitted_at", "IS NOT DISTINCT FROM", "job_rows_pending_flag_is_verify_only", "suppressed"}},
		{"job_results_after_confirm", []string{"jobs_results_after_confirm", "platform.jobs", "'draft'", "'previewed'", "suppressed"}},
		{"job_unit_prices_only_reachable", []string{"snapshot_only_reachable_units", "platform.unit_costs", "reachable"}},
		{"unit_costs_record_reachability", []string{"platform.unit_reachability_history", "reachable_source"}},
		{"set_unit_reachable", []string{"22023", "P0002", "FOR UPDATE", "platform.unit_costs", "reachable_source", "reachable_changed_at"}},
		{"jobs_guard", []string{"never change", "frozen", "IS DISTINCT FROM", "'draft'", "'previewed'", "preview_fingerprint", "confirm_key", "frozen_settings"}},
	} {
		rows, err := phase2Rows(ctx, q, `SELECT p.prosrc FROM pg_proc p WHERE p.pronamespace = 'platform'::regnamespace AND p.proname::text = $1`, b.fn)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			continue // reported as missing by the function check above
		}
		for _, tok := range b.tokens {
			if !strings.Contains(rows[0][0], tok) {
				add("platform.%s(): its body does not mention %q (a no-op body would keep the trigger and every catalog fact above)", b.fn, tok)
			}
		}
	}

	// 5c. the unique indexes of platform_0005: one charge per (job, item), one reserve per job, one release per reserve - each a UNIQUE index of platform.ledger over exactly its columns and
	// partial over exactly its kind
	for _, ix := range []struct{ name, cols, kind string }{
		{"ledger_one_charge_per_item", "(job_id, item_ref)", "charge"},
		{"ledger_one_reserve_per_job", "(job_id)", "reserve"},
		{"ledger_one_release_per_reserve", "(reserve_id)", "release"},
	} {
		rows, err := phase2Rows(ctx, q, `SELECT i.indisunique::text, COALESCE(pg_get_expr(i.indpred, i.indrelid), ''), pg_get_indexdef(i.indexrelid), (i.indrelid = to_regclass('platform.ledger'))::text
			FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relnamespace = 'platform'::regnamespace AND c.relname::text = $1`, ix.name)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			add("platform.%s: missing (a unique partial index of platform.ledger %s where kind = '%s')", ix.name, ix.cols, ix.kind)
			continue
		}
		r := rows[0]
		if r[0] != "true" {
			add("platform.%s: not unique", ix.name)
		}
		if r[3] != "true" {
			add("platform.%s: not an index of platform.ledger", ix.name)
		}
		if !strings.Contains(r[2], ix.cols) {
			add("platform.%s: not over %s: %s", ix.name, ix.cols, r[2])
		}
		if got, want := phase2Literals(r[1]), []string{ix.kind}; strings.Join(got, ",") != strings.Join(want, ",") {
			add("platform.%s: its predicate covers the kinds %v, want exactly %v", ix.name, got, want)
		}
	}

	// 6. the view
	rows, err = phase2Rows(ctx, q, `SELECT c.relkind::text, COALESCE((SELECT string_agg(a.attname::text, ',' ORDER BY a.attnum) FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped), ''), pg_get_viewdef(c.oid)
		FROM pg_class c WHERE c.relnamespace = 'platform'::regnamespace AND c.relname::text = 'listable_users'`)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		add("platform.listable_users: missing (the only sanctioned way to list users)")
	} else {
		r := rows[0]
		if r[0] != "v" {
			add("platform.listable_users: is relkind %q, want a view (v)", r[0])
		}
		if r[1] != "id,auth_user_id,email,is_active,created_at" {
			add("platform.listable_users: its columns are %s, want id,auth_user_id,email,is_active,created_at", r[1])
		}
		if !strings.Contains(r[2], phase2ZeroUUID) {
			add("platform.listable_users: the view does not exclude the system user (%s is not in its definition)", phase2ZeroUUID)
		}
		if !strings.Contains(r[2], "auth_user_id IS NOT NULL") {
			add("platform.listable_users: the view does not exclude users with no auth link (no `auth_user_id IS NOT NULL` in its definition)")
		}
	}
	return v, nil
}

// phase2After runs setup (as the superuser, in a transaction that is rolled back) and returns what phase2CatalogCheck says about the result.
func phase2After(t *testing.T, db shapeDB, setup []string) []string {
	t.Helper()
	ctx := testCtx(t)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, s := range setup {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	got, err := phase2CatalogCheck(ctx, tx)
	if err != nil {
		t.Fatalf("the checker could not run on the broken database: %v", err)
	}
	return got
}

// TestSchemaLocks_Phase2Rulings pins what platform_0004 leaves in the catalog (see phase2CatalogCheck): clean on a freshly migrated scratch database and on the live one, and each
// breakage below - a dropped, weakened, renamed, disabled or replaced object - is reported by the object's name. It fails when a later migration removes or weakens any of them.
func TestSchemaLocks_Phase2Rulings(t *testing.T) {
	_, admin := migratedScratch(t)
	ctx := testCtx(t)
	v, err := phase2CatalogCheck(ctx, admin)
	requireNoViolations(t, v, err, "a freshly migrated scratch database")

	t.Run("the live database holds them too", func(t *testing.T) {
		v, err := phase2CatalogCheck(testCtx(t), liveAdmin(t))
		requireNoViolations(t, v, err, "the live database")
	})

	slot := "platform.jobs (user_id, module_id)"
	cases := []struct {
		name  string
		setup []string
		want  [][]string // each entry: substrings that must all appear in one violation
	}{
		{"the slot index dropped",
			[]string{"DROP INDEX platform.jobs_one_active_per_user_module"},
			[][]string{{"jobs_one_active_per_user_module", "missing"}}},
		{"the old slot index back beside the new one",
			[]string{"CREATE UNIQUE INDEX jobs_one_executing_per_user_module ON " + slot + " WHERE status IN ('running', 'paused', 'cancelling')"},
			[][]string{{"jobs_one_executing_per_user_module", "OLD slot index"}}},
		{"the slot index without 'scheduled' (Phase 1's rule, with the states of platform_0011 around it)",
			[]string{"DROP INDEX platform.jobs_one_active_per_user_module", "CREATE UNIQUE INDEX jobs_one_active_per_user_module ON " + slot + " WHERE status IN ('confirmed', 'running', 'paused', 'cancelling', 'completing')"},
			[][]string{{"jobs_one_active_per_user_module", "covers the statuses [cancelling completing confirmed paused running]"}}},
		{"the slot index of platform_0004 (before platform_0011 widened it: no confirmed, no completing)",
			[]string{"DROP INDEX platform.jobs_one_active_per_user_module", "CREATE UNIQUE INDEX jobs_one_active_per_user_module ON " + slot + " WHERE status IN ('scheduled', 'running', 'paused', 'cancelling')"},
			[][]string{{"jobs_one_active_per_user_module", "covers the statuses [cancelling paused running scheduled]"}}},
		{"the slot index without 'confirmed' (a confirmed job holds credits and the slot)",
			[]string{"DROP INDEX platform.jobs_one_active_per_user_module", "CREATE UNIQUE INDEX jobs_one_active_per_user_module ON " + slot + " WHERE status IN ('scheduled', 'running', 'paused', 'cancelling', 'completing')"},
			[][]string{{"jobs_one_active_per_user_module", "covers the statuses [cancelling completing paused running scheduled]"}}},
		{"the slot index without 'completing' (the module has finished, the platform is still recording)",
			[]string{"DROP INDEX platform.jobs_one_active_per_user_module", "CREATE UNIQUE INDEX jobs_one_active_per_user_module ON " + slot + " WHERE status IN ('confirmed', 'scheduled', 'running', 'paused', 'cancelling')"},
			[][]string{{"jobs_one_active_per_user_module", "covers the statuses [cancelling confirmed paused running scheduled]"}}},
		{"the slot index that also holds a previewed job (a preview holds nothing)",
			[]string{"DROP INDEX platform.jobs_one_active_per_user_module", "CREATE UNIQUE INDEX jobs_one_active_per_user_module ON " + slot + " WHERE status IN ('previewed', 'confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing')"},
			[][]string{{"jobs_one_active_per_user_module", "covers the statuses [cancelling completing confirmed paused previewed running scheduled]"}}},
		{"the slot index not unique",
			[]string{"DROP INDEX platform.jobs_one_active_per_user_module", "CREATE INDEX jobs_one_active_per_user_module ON " + slot + " WHERE status IN ('confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing')"},
			[][]string{{"jobs_one_active_per_user_module", "not unique"}}},
		{"the slot index over one column",
			[]string{"DROP INDEX platform.jobs_one_active_per_user_module", "CREATE UNIQUE INDEX jobs_one_active_per_user_module ON platform.jobs (user_id) WHERE status IN ('confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing')"},
			[][]string{{"jobs_one_active_per_user_module", "not over (user_id, module_id)"}}},
		{"the four system-user CHECKs dropped",
			[]string{"ALTER TABLE platform.balances DROP CONSTRAINT balances_not_the_system_user", "ALTER TABLE platform.ledger DROP CONSTRAINT ledger_not_the_system_user",
				"ALTER TABLE platform.m3_entitlements DROP CONSTRAINT m3_entitlements_not_the_system_user", "ALTER TABLE platform.suppressions DROP CONSTRAINT suppressions_not_the_system_user"},
			[][]string{{"balances_not_the_system_user", "missing"}, {"ledger_not_the_system_user", "missing"}, {"m3_entitlements_not_the_system_user", "missing"}, {"suppressions_not_the_system_user", "missing"}}},
		{"a system-user CHECK that names another uuid",
			[]string{"ALTER TABLE platform.ledger DROP CONSTRAINT ledger_not_the_system_user",
				"ALTER TABLE platform.ledger ADD CONSTRAINT ledger_not_the_system_user CHECK (user_id <> '11111111-1111-1111-1111-111111111111'::uuid)"},
			[][]string{{"ledger_not_the_system_user", "does not name the all-zero system user"}}},
		{"ledger_shape_adjustment dropped",
			[]string{"ALTER TABLE platform.ledger DROP CONSTRAINT ledger_shape_adjustment"},
			[][]string{{"ledger_shape_adjustment", "missing"}}},
		{"ledger_shape_adjustment that lets an adjustment move reserved credits",
			[]string{"ALTER TABLE platform.ledger DROP CONSTRAINT ledger_shape_adjustment",
				"ALTER TABLE platform.ledger ADD CONSTRAINT ledger_shape_adjustment CHECK (kind <> 'adjustment' OR (available_delta < 0 AND job_id IS NULL AND reserve_id IS NULL AND unit IS NULL AND units IS NULL AND unit_cost IS NULL AND acting_admin_id IS NOT NULL AND reason IS NOT NULL AND btrim(reason) <> ''))"},
			[][]string{{"ledger_shape_adjustment", "lacks", "reserved_delta = 0"}}},
		{"the kind vocabulary without adjustment",
			[]string{"ALTER TABLE platform.ledger DROP CONSTRAINT ledger_kind_vocabulary",
				"ALTER TABLE platform.ledger ADD CONSTRAINT ledger_kind_vocabulary CHECK (kind IN ('grant', 'reserve', 'charge', 'release', 'm3_enable', 'm3_disable', 'm3_expire'))"},
			[][]string{{"ledger_kind_vocabulary", "lacks", "'adjustment'"}}},
		{"the ledger audit trigger disabled",
			[]string{"ALTER TABLE platform.ledger DISABLE TRIGGER ledger_audit_adjustment"},
			[][]string{{"ledger_audit_adjustment", "not enabled"}}},
		{"a disabled-admin trigger dropped",
			[]string{"DROP TRIGGER m3_entitlements_require_active_admin ON platform.m3_entitlements"},
			[][]string{{"m3_entitlements_require_active_admin", "missing"}}},
		{"a disabled-admin trigger that fires AFTER the row is written",
			[]string{"DROP TRIGGER admin_audit_require_active_admin ON platform.admin_audit",
				"CREATE TRIGGER admin_audit_require_active_admin AFTER INSERT ON platform.admin_audit FOR EACH ROW EXECUTE FUNCTION platform.admin_audit_require_active_admin()"},
			[][]string{{"admin_audit_require_active_admin", "fires as AFTER INSERT", "want BEFORE INSERT"}}},
		{"the entitlement trigger that forgets UPDATE",
			[]string{"DROP TRIGGER m3_entitlements_require_active_admin ON platform.m3_entitlements",
				"CREATE TRIGGER m3_entitlements_require_active_admin BEFORE INSERT ON platform.m3_entitlements FOR EACH ROW EXECUTE FUNCTION platform.m3_entitlements_require_active_admin()"},
			[][]string{{"m3_entitlements_require_active_admin", "fires as BEFORE INSERT FOR EACH ROW", "BEFORE INSERT OR UPDATE"}}},
		{"the audit trigger firing for every kind (no WHEN)",
			[]string{"DROP TRIGGER ledger_audit_adjustment ON platform.ledger",
				"CREATE TRIGGER ledger_audit_adjustment AFTER INSERT ON platform.ledger FOR EACH ROW EXECUTE FUNCTION platform.ledger_audit_adjustment()"},
			[][]string{{"ledger_audit_adjustment", "WHEN clause present = false"}}},
		{"unit_costs_guard without the admin check",
			[]string{"CREATE OR REPLACE FUNCTION platform.unit_costs_guard() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$ BEGIN RETURN NEW; END $$"},
			[][]string{{"unit_costs_guard", "does not call require_active_admin"}}},
		{"the admin check dropped",
			[]string{"DROP FUNCTION platform.require_active_admin(uuid, text)"},
			[][]string{{"require_active_admin(uuid, text)", "missing"}}},
		{"the admin check without a pinned search_path (a definer anyone could hijack)",
			[]string{"ALTER FUNCTION platform.require_active_admin(uuid, text) RESET search_path"},
			[][]string{{"require_active_admin(uuid, text)", "does not pin search_path"}}},
		{"the admin check no longer a definer (the roles could not read platform.admins FOR SHARE)",
			[]string{"ALTER FUNCTION platform.require_active_admin(uuid, text) SECURITY INVOKER"},
			[][]string{{"require_active_admin(uuid, text)", "is not SECURITY DEFINER"}}},
		{"monetary without the adjustment kind (the balance trigger and balance_drift would ignore a clawback)",
			[]string{"DROP VIEW platform.balance_drift", "ALTER TABLE platform.ledger DROP CONSTRAINT ledger_module_metering_fk", "ALTER TABLE platform.ledger DROP COLUMN monetary",
				"ALTER TABLE platform.ledger ADD COLUMN monetary boolean GENERATED ALWAYS AS (kind IN ('grant', 'reserve', 'charge', 'release')) STORED"},
			[][]string{{"platform.ledger.monetary", "adjustment"}, {"ledger_module_metering_fk", "missing"}}},
		{"the module-metering key over the wrong columns",
			[]string{"ALTER TABLE platform.ledger DROP CONSTRAINT ledger_module_metering_fk", "ALTER TABLE platform.ledger ADD CONSTRAINT ledger_module_metering_fk FOREIGN KEY (module_id) REFERENCES platform.modules (id)"},
			[][]string{{"ledger_module_metering_fk", "want a foreign key (module_id,monetary)"}}},
		{"the user view dropped",
			[]string{"DROP VIEW platform.listable_users"},
			[][]string{{"platform.listable_users", "missing"}}},
		{"the user view that lists everyone",
			[]string{"CREATE OR REPLACE VIEW platform.listable_users AS SELECT id, auth_user_id, email, is_active, created_at FROM public.users"},
			[][]string{{"platform.listable_users", "does not exclude the system user"}, {"platform.listable_users", "does not exclude users with no auth link"}}},
		{"the user view that only drops the system user",
			[]string{"CREATE OR REPLACE VIEW platform.listable_users AS SELECT id, auth_user_id, email, is_active, created_at FROM public.users WHERE id <> '00000000-0000-0000-0000-000000000000'::uuid"},
			[][]string{{"platform.listable_users", "does not exclude users with no auth link"}}},

		// ---- platform_0005: the money core ----
		{"the item_ref shape dropped (a charge could carry no item)",
			[]string{"ALTER TABLE platform.ledger DROP CONSTRAINT ledger_item_ref_shape"},
			[][]string{{"ledger_item_ref_shape", "missing"}}},
		{"the item_ref shape without the length bound",
			[]string{"ALTER TABLE platform.ledger DROP CONSTRAINT ledger_item_ref_shape",
				"ALTER TABLE platform.ledger ADD CONSTRAINT ledger_item_ref_shape CHECK ((kind = 'charge' AND item_ref IS NOT NULL AND btrim(item_ref) <> '') OR (kind <> 'charge' AND item_ref IS NULL))"},
			[][]string{{"ledger_item_ref_shape", "lacks", "octet_length(item_ref) <= 200"}}},
		{"the one-item rule weakened to at least one unit",
			[]string{"ALTER TABLE platform.ledger DROP CONSTRAINT ledger_charge_is_one_item",
				"ALTER TABLE platform.ledger ADD CONSTRAINT ledger_charge_is_one_item CHECK (kind <> 'charge' OR COALESCE(units >= 1, false))"},
			[][]string{{"ledger_charge_is_one_item", "lacks", "units = 1"}}},
		{"the per-reserve cap dropped",
			[]string{"ALTER TABLE platform.reserves DROP CONSTRAINT reserves_not_overspent"},
			[][]string{{"platform.reserves.reserves_not_overspent", "missing"}}},
		{"the one-charge-per-item index dropped",
			[]string{"DROP INDEX platform.ledger_one_charge_per_item"},
			[][]string{{"ledger_one_charge_per_item", "missing"}}},
		{"the one-reserve-per-job index not unique",
			[]string{"DROP INDEX platform.ledger_one_reserve_per_job", "CREATE INDEX ledger_one_reserve_per_job ON platform.ledger (job_id) WHERE kind = 'reserve'"},
			[][]string{{"ledger_one_reserve_per_job", "not unique"}}},
		{"the one-release-per-reserve index over every kind (no predicate)",
			[]string{"DROP INDEX platform.ledger_one_release_per_reserve", "CREATE UNIQUE INDEX ledger_one_release_per_reserve ON platform.ledger (reserve_id)"},
			[][]string{{"ledger_one_release_per_reserve", "its predicate covers"}}},
		{"the one-charge-per-item index over the job only",
			[]string{"DROP INDEX platform.ledger_one_charge_per_item", "CREATE UNIQUE INDEX ledger_one_charge_per_item ON platform.ledger (job_id) WHERE kind = 'charge'"},
			[][]string{{"ledger_one_charge_per_item", "not over (job_id, item_ref)"}}},
		{"the reserve rate trigger dropped",
			[]string{"DROP TRIGGER ledger_check_reserve ON platform.ledger"},
			[][]string{{"ledger_check_reserve", "missing"}}},
		{"the reserve rate trigger that fires for every kind (no WHEN)",
			[]string{"DROP TRIGGER ledger_check_reserve ON platform.ledger",
				"CREATE TRIGGER ledger_check_reserve BEFORE INSERT ON platform.ledger FOR EACH ROW EXECUTE FUNCTION platform.ledger_check_reserve()"},
			[][]string{{"ledger_check_reserve", "WHEN clause present = false"}}},
		{"the reserve rate trigger disabled",
			[]string{"ALTER TABLE platform.ledger DISABLE TRIGGER ledger_check_reserve"},
			[][]string{{"ledger_check_reserve", "not enabled"}}},
		{"the reserve rate trigger turned into a no-op (it is still there, still a definer, still pinned)",
			[]string{"CREATE OR REPLACE FUNCTION platform.ledger_check_reserve() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$ BEGIN RETURN NEW; END $$"},
			[][]string{{"ledger_check_reserve()", "its body does not mention", "reserve_at_the_dearest_price"}, {"ledger_check_reserve()", "reserve_needs_the_complete_snapshot"}}},
		{"the release trigger that fires AFTER the row is written",
			[]string{"DROP TRIGGER ledger_check_release ON platform.ledger",
				"CREATE TRIGGER ledger_check_release AFTER INSERT ON platform.ledger FOR EACH ROW WHEN (NEW.kind = 'release') EXECUTE FUNCTION platform.ledger_check_release()"},
			[][]string{{"ledger_check_release", "fires as AFTER INSERT", "want BEFORE INSERT"}}},
		{"the release check turned into a no-op",
			[]string{"CREATE OR REPLACE FUNCTION platform.ledger_check_release() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$ BEGIN RETURN NEW; END $$"},
			[][]string{{"ledger_check_release()", "its body does not mention", "release_is_the_remainder"}}},
		{"the reserves writer dropped (nothing would track a reserve)",
			[]string{"DROP TRIGGER ledger_apply_to_reserves ON platform.ledger"},
			[][]string{{"ledger_apply_to_reserves", "missing"}}},
		{"the reserves writer without its WHEN (it would fire for every kind)",
			[]string{"DROP TRIGGER ledger_apply_to_reserves ON platform.ledger",
				"CREATE TRIGGER ledger_apply_to_reserves AFTER INSERT ON platform.ledger FOR EACH ROW EXECUTE FUNCTION platform.ledger_apply_to_reserves()"},
			[][]string{{"ledger_apply_to_reserves", "WHEN clause present = false"}}},
		{"the reserves writer no longer a definer (the roles cannot write platform.reserves)",
			[]string{"ALTER FUNCTION platform.ledger_apply_to_reserves() SECURITY INVOKER"},
			[][]string{{"ledger_apply_to_reserves()", "is not SECURITY DEFINER"}}},
		{"the reserves writer that silently skips a charge whose reserve has no row (the older body)",
			[]string{"CREATE OR REPLACE FUNCTION platform.ledger_apply_to_reserves() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$ BEGIN " +
				"IF NEW.kind = 'reserve' THEN INSERT INTO platform.reserves (reserve_id, user_id, module_id, job_id, reserved) VALUES (NEW.id, NEW.user_id, NEW.module_id, NEW.job_id, NEW.reserved_delta); " +
				"ELSIF NEW.kind = 'charge' THEN UPDATE platform.reserves SET charged = charged - NEW.reserved_delta WHERE reserve_id = NEW.reserve_id; " +
				"ELSE UPDATE platform.reserves SET released = released + NEW.available_delta WHERE reserve_id = NEW.reserve_id; END IF; RETURN NULL; END $$"},
			[][]string{{"ledger_apply_to_reserves()", "its body does not mention", "reserve_row_exists"}}},

		// ---- platform_0005: the ledger's clock is the database's ----
		{"the created_at stamp dropped (a writer could supply the time again)",
			[]string{"DROP TRIGGER ledger_stamp_created_at ON platform.ledger"},
			[][]string{{"ledger_stamp_created_at", "missing"}}},
		{"the created_at stamp disabled",
			[]string{"ALTER TABLE platform.ledger DISABLE TRIGGER ledger_stamp_created_at"},
			[][]string{{"ledger_stamp_created_at", "not enabled"}}},
		{"the created_at stamp that fires AFTER the row is written (too late to matter)",
			[]string{"DROP TRIGGER ledger_stamp_created_at ON platform.ledger",
				"CREATE TRIGGER ledger_stamp_created_at AFTER INSERT ON platform.ledger FOR EACH ROW EXECUTE FUNCTION platform.ledger_stamp_created_at()"},
			[][]string{{"ledger_stamp_created_at", "fires as AFTER INSERT", "want BEFORE INSERT"}}},
		{"the created_at stamp that only fires for one kind (a WHEN clause)",
			[]string{"DROP TRIGGER ledger_stamp_created_at ON platform.ledger",
				"CREATE TRIGGER ledger_stamp_created_at BEFORE INSERT ON platform.ledger FOR EACH ROW WHEN (NEW.kind = 'm3_expire') EXECUTE FUNCTION platform.ledger_stamp_created_at()"},
			[][]string{{"ledger_stamp_created_at", "WHEN clause present = true"}}},
		{"the created_at stamp turned into a no-op (the trigger, the owner and the search_path are all still there)",
			[]string{"CREATE OR REPLACE FUNCTION platform.ledger_stamp_created_at() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$ BEGIN RETURN NEW; END $$"},
			[][]string{{"ledger_stamp_created_at()", "its body does not mention", "now()"}}},

		// ---- platform_0006: the entitlement flow ----
		{"the until_at rule dropped",
			[]string{"ALTER TABLE platform.ledger DROP CONSTRAINT ledger_until_at_shape"},
			[][]string{{"ledger_until_at_shape", "missing"}}},
		{"the until_at rule that lets any kind carry a date",
			[]string{"ALTER TABLE platform.ledger DROP CONSTRAINT ledger_until_at_shape", "ALTER TABLE platform.ledger ADD CONSTRAINT ledger_until_at_shape CHECK (until_at IS NULL OR until_at > created_at)"},
			[][]string{{"ledger_until_at_shape", "lacks", "'m3_enable'"}}},
		{"the entitlement writer dropped (the entitlement would no longer follow the ledger)",
			[]string{"DROP TRIGGER ledger_apply_to_entitlements ON platform.ledger"},
			[][]string{{"ledger_apply_to_entitlements", "missing"}}},
		{"the entitlement writer turned into a no-op",
			[]string{"CREATE OR REPLACE FUNCTION platform.ledger_apply_to_entitlements() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$ BEGIN RETURN NULL; END $$"},
			[][]string{{"ledger_apply_to_entitlements()", "its body does not mention", "m3_expire_is_due"}}},
		{"the entitlement writer without its pinned search_path (a definer anyone could hijack)",
			[]string{"ALTER FUNCTION platform.ledger_apply_to_entitlements() RESET search_path"},
			[][]string{{"ledger_apply_to_entitlements()", "does not pin search_path"}}},
		{"the delivery-row trigger back to a bare UPDATE (a missing delivery row would be skipped in silence)",
			[]string{"CREATE OR REPLACE FUNCTION platform.m3_entitlements_raise_sync() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$ BEGIN " +
				"IF TG_OP = 'INSERT' THEN INSERT INTO platform.m3_sync (user_id, want_suspended) VALUES (NEW.user_id, NOT NEW.enabled); " +
				"ELSIF NEW.enabled IS DISTINCT FROM OLD.enabled THEN UPDATE platform.m3_sync SET want_suspended = NOT NEW.enabled, want_version = want_version + 1 WHERE user_id = NEW.user_id; END IF; RETURN NULL; END $$"},
			[][]string{{"m3_entitlements_raise_sync()", "its body does not mention", "ON CONFLICT"}}},
		{"the delivery-row trigger that forgets UPDATE (a later disable would never raise want_version)",
			[]string{"DROP TRIGGER m3_entitlements_raise_sync ON platform.m3_entitlements",
				"CREATE TRIGGER m3_entitlements_raise_sync AFTER INSERT ON platform.m3_entitlements FOR EACH ROW EXECUTE FUNCTION platform.m3_entitlements_raise_sync()"},
			[][]string{{"m3_entitlements_raise_sync", "fires as AFTER INSERT FOR EACH ROW", "AFTER INSERT OR UPDATE"}}},
		{"the convergence rule dropped (converged could be recorded for a state module 3 was never seen in)",
			[]string{"ALTER TABLE platform.m3_sync DROP CONSTRAINT m3_sync_converged_only_when_observed_at_version"},
			[][]string{{"platform.m3_sync.m3_sync_converged_only_when_observed_at_version", "missing"}}},
		{"the convergence rule that ignores what was observed",
			[]string{"ALTER TABLE platform.m3_sync DROP CONSTRAINT m3_sync_converged_only_when_observed_at_version",
				"ALTER TABLE platform.m3_sync ADD CONSTRAINT m3_sync_converged_only_when_observed_at_version CHECK (converged_version < want_version OR (observed_version IS NOT NULL AND observed_version = want_version AND want_suspended))"},
			[][]string{{"m3_sync_converged_only_when_observed_at_version", "lacks", "'active'"}}},
		{"the convergence rule that ignores the version the observation was made at (the platform_0006 rule)",
			[]string{"ALTER TABLE platform.m3_sync DROP CONSTRAINT m3_sync_converged_only_when_observed_at_version",
				"ALTER TABLE platform.m3_sync ADD CONSTRAINT m3_sync_converged_only_when_observed_at_version CHECK (converged_version < want_version OR (want_suspended AND observed = 'suspended') OR (NOT want_suspended AND observed = 'active'))"},
			[][]string{{"m3_sync_converged_only_when_observed_at_version", "lacks", "observed_version = want_version"}}},
		{"the convergence rule that lets a NULL version through (the CHECK would evaluate to NULL and pass)",
			[]string{"ALTER TABLE platform.m3_sync DROP CONSTRAINT m3_sync_converged_only_when_observed_at_version",
				"ALTER TABLE platform.m3_sync ADD CONSTRAINT m3_sync_converged_only_when_observed_at_version CHECK (converged_version < want_version OR (observed_version = want_version AND ((want_suspended AND observed = 'suspended') OR (NOT want_suspended AND observed = 'active'))))"},
			[][]string{{"m3_sync_converged_only_when_observed_at_version", "lacks", "observed_version IS NOT NULL"}}},
		{"the converged-not-ahead rule dropped",
			[]string{"ALTER TABLE platform.m3_sync DROP CONSTRAINT m3_sync_converged_not_ahead"},
			[][]string{{"m3_sync_converged_not_ahead", "missing"}}},
		{"the observed vocabulary dropped",
			[]string{"ALTER TABLE platform.m3_sync DROP CONSTRAINT m3_sync_observed_vocabulary"},
			[][]string{{"m3_sync_observed_vocabulary", "missing"}}},

		// ---- platform_0007: snapshot prices and terminal job states ----
		{"the snapshot price trigger dropped (a forged price would be accepted again)",
			[]string{"DROP TRIGGER job_unit_prices_snapshot_equals_unit_costs ON platform.job_unit_prices"},
			[][]string{{"job_unit_prices_snapshot_equals_unit_costs", "missing"}}},
		{"the snapshot price trigger disabled",
			[]string{"ALTER TABLE platform.job_unit_prices DISABLE TRIGGER job_unit_prices_snapshot_equals_unit_costs"},
			[][]string{{"job_unit_prices_snapshot_equals_unit_costs", "not enabled"}}},
		{"the snapshot price trigger that fires AFTER the row is written (the CHECK and the keys would answer first)",
			[]string{"DROP TRIGGER job_unit_prices_snapshot_equals_unit_costs ON platform.job_unit_prices",
				"CREATE TRIGGER job_unit_prices_snapshot_equals_unit_costs AFTER INSERT ON platform.job_unit_prices FOR EACH ROW EXECUTE FUNCTION platform.job_unit_prices_snapshot_equals_unit_costs()"},
			[][]string{{"job_unit_prices_snapshot_equals_unit_costs", "fires as AFTER INSERT", "want BEFORE INSERT"}}},
		{"the snapshot price trigger turned into a no-op (the trigger, the owner and the search_path are all still there)",
			[]string{"CREATE OR REPLACE FUNCTION platform.job_unit_prices_snapshot_equals_unit_costs() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$ BEGIN RETURN NEW; END $$"},
			[][]string{{"job_unit_prices_snapshot_equals_unit_costs()", "its body does not mention", "snapshot_equals_unit_costs"}}},
		{"the snapshot price check without a pinned search_path",
			[]string{"ALTER FUNCTION platform.job_unit_prices_snapshot_equals_unit_costs() RESET search_path"},
			[][]string{{"job_unit_prices_snapshot_equals_unit_costs()", "does not pin search_path"}}},
		{"the terminal-state trigger dropped (a succeeded job could go back to draft)",
			[]string{"DROP TRIGGER jobs_terminal_is_final ON platform.jobs"},
			[][]string{{"platform.jobs trigger jobs_terminal_is_final", "missing"}}},
		{"the terminal-state trigger disabled",
			[]string{"ALTER TABLE platform.jobs DISABLE TRIGGER jobs_terminal_is_final"},
			[][]string{{"jobs_terminal_is_final", "not enabled"}}},
		{"the terminal-state trigger that fires AFTER the row is written",
			[]string{"DROP TRIGGER jobs_terminal_is_final ON platform.jobs",
				"CREATE TRIGGER jobs_terminal_is_final AFTER UPDATE ON platform.jobs FOR EACH ROW EXECUTE FUNCTION platform.jobs_terminal_is_final()"},
			[][]string{{"jobs_terminal_is_final", "fires as AFTER UPDATE", "want BEFORE UPDATE"}}},
		{"the terminal-state trigger that only fires for one status (a WHEN clause)",
			[]string{"DROP TRIGGER jobs_terminal_is_final ON platform.jobs",
				"CREATE TRIGGER jobs_terminal_is_final BEFORE UPDATE ON platform.jobs FOR EACH ROW WHEN (OLD.status = 'succeeded') EXECUTE FUNCTION platform.jobs_terminal_is_final()"},
			[][]string{{"jobs_terminal_is_final", "WHEN clause present = true"}}},
		{"the terminal-state rule turned into a no-op",
			[]string{"CREATE OR REPLACE FUNCTION platform.jobs_terminal_is_final() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$ BEGIN RETURN NEW; END $$"},
			[][]string{{"jobs_terminal_is_final()", "its body does not mention", "jobs_terminal_is_final"}}},
		{"the terminal-state rule that holds only a succeeded job (failed and cancelled could move)",
			[]string{"CREATE OR REPLACE FUNCTION platform.jobs_terminal_is_final() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$ BEGIN " +
				"IF OLD.status = 'succeeded' AND NEW.status IS DISTINCT FROM OLD.status THEN RAISE EXCEPTION 'x' USING ERRCODE = '23001', CONSTRAINT = 'jobs_terminal_is_final'; END IF; RETURN NEW; END $$"},
			[][]string{{"jobs_terminal_is_final()", "its body does not mention", "'failed'"}, {"jobs_terminal_is_final()", "its body does not mention", "'cancelled'"}}},

		// ---- platform_0011: the lifecycle of a job ----
		{"the terminal-state rule that does not know expired (an expired job could move)",
			[]string{"CREATE OR REPLACE FUNCTION platform.jobs_terminal_is_final() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$ BEGIN " +
				"IF OLD.status IN ('succeeded', 'failed', 'cancelled') AND NEW.status IS DISTINCT FROM OLD.status THEN RAISE EXCEPTION 'x' USING ERRCODE = '23001', CONSTRAINT = 'jobs_terminal_is_final'; END IF; RETURN NEW; END $$"},
			[][]string{{"jobs_terminal_is_final()", "its body does not mention", "'expired'"}}},
		{"the lifecycle guard dropped (a job could be born running and move anywhere)",
			[]string{"DROP TRIGGER jobs_transition_guard ON platform.jobs"},
			[][]string{{"platform.jobs trigger jobs_transition_guard", "missing"}}},
		{"the lifecycle guard disabled",
			[]string{"ALTER TABLE platform.jobs DISABLE TRIGGER jobs_transition_guard"},
			[][]string{{"jobs_transition_guard", "not enabled"}}},
		{"the lifecycle guard that fires only for an UPDATE (a job could be born running)",
			[]string{"DROP TRIGGER jobs_transition_guard ON platform.jobs",
				"CREATE TRIGGER jobs_transition_guard BEFORE UPDATE ON platform.jobs FOR EACH ROW EXECUTE FUNCTION platform.jobs_transition_guard()"},
			[][]string{{"jobs_transition_guard", "fires as BEFORE UPDATE FOR EACH ROW", "want BEFORE INSERT OR UPDATE"}}},
		{"the lifecycle guard that fires AFTER the row is written (the CHECKs and the slot would answer first)",
			[]string{"DROP TRIGGER jobs_transition_guard ON platform.jobs",
				"CREATE TRIGGER jobs_transition_guard AFTER INSERT OR UPDATE ON platform.jobs FOR EACH ROW EXECUTE FUNCTION platform.jobs_transition_guard()"},
			[][]string{{"jobs_transition_guard", "fires as AFTER INSERT OR UPDATE", "want BEFORE INSERT OR UPDATE"}}},
		{"the lifecycle guard turned into a no-op (the trigger, the owner and the search_path are all still there)",
			[]string{"CREATE OR REPLACE FUNCTION platform.jobs_transition_guard() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$ BEGIN RETURN NEW; END $$"},
			[][]string{{"jobs_transition_guard()", "its body does not mention", "job_transition_illegal"}, {"jobs_transition_guard()", "its body does not mention", "job_born_draft"}}},
		{"the lifecycle guard without a pinned search_path",
			[]string{"ALTER FUNCTION platform.jobs_transition_guard() RESET search_path"},
			[][]string{{"jobs_transition_guard()", "does not pin search_path"}}},
		{"the event writer dropped (a move would leave no trace)",
			[]string{"DROP TRIGGER jobs_record_transition ON platform.jobs"},
			[][]string{{"platform.jobs trigger jobs_record_transition", "missing"}}},
		{"the event writer that fires for every UPDATE (no WHEN: a status written back would be an event)",
			[]string{"DROP TRIGGER jobs_record_transition ON platform.jobs",
				"CREATE TRIGGER jobs_record_transition AFTER UPDATE OF status ON platform.jobs FOR EACH ROW EXECUTE FUNCTION platform.jobs_record_transition()"},
			[][]string{{"jobs_record_transition", "WHEN clause present = false"}}},
		{"the event actor check dropped (a disabled admin could be named in the history)",
			[]string{"DROP TRIGGER job_events_check_actor ON platform.job_events"},
			[][]string{{"platform.job_events trigger job_events_check_actor", "missing"}}},
		{"the sanctioned move made SECURITY DEFINER (it would lend the owner's UPDATE to whoever may call it)",
			[]string{"ALTER FUNCTION platform.job_transition(uuid, text, text, uuid, text) SECURITY DEFINER"},
			[][]string{{"platform.job_transition()", "SECURITY DEFINER", "want SECURITY INVOKER"}}},
		{"the sanctioned move without a pinned search_path",
			[]string{"ALTER FUNCTION platform.job_transition(uuid, text, text, uuid, text) RESET search_path"},
			[][]string{{"job_transition(uuid, text, text, uuid, text)", "does not pin search_path"}}},
		{"the status vocabulary without expired",
			[]string{"ALTER TABLE platform.jobs DROP CONSTRAINT jobs_status_vocabulary",
				"ALTER TABLE platform.jobs ADD CONSTRAINT jobs_status_vocabulary CHECK (status IN ('draft', 'previewed', 'confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing', 'succeeded', 'failed', 'cancelled'))"},
			[][]string{{"platform.jobs.jobs_status_vocabulary", "lacks", "'expired'"}}},
		{"the confirmed-shape CHECK dropped (a running job without a preview)",
			[]string{"ALTER TABLE platform.jobs DROP CONSTRAINT jobs_confirmed_shape"},
			[][]string{{"platform.jobs.jobs_confirmed_shape", "missing"}}},
		{"the confirmed-shape CHECK that forgets the fingerprint",
			[]string{"ALTER TABLE platform.jobs DROP CONSTRAINT jobs_confirmed_shape",
				"ALTER TABLE platform.jobs ADD CONSTRAINT jobs_confirmed_shape CHECK (status NOT IN ('confirmed', 'scheduled', 'running', 'paused', 'cancelling', 'completing') OR (preview IS NOT NULL AND confirm_key IS NOT NULL AND confirmed_at IS NOT NULL))"},
			[][]string{{"platform.jobs.jobs_confirmed_shape", "lacks", "preview_fingerprint IS NOT NULL"}}},
		{"the confirmed-shape CHECK that does not cover completing",
			[]string{"ALTER TABLE platform.jobs DROP CONSTRAINT jobs_confirmed_shape",
				"ALTER TABLE platform.jobs ADD CONSTRAINT jobs_confirmed_shape CHECK (status NOT IN ('confirmed', 'scheduled', 'running', 'paused', 'cancelling') OR (preview IS NOT NULL AND preview_fingerprint IS NOT NULL AND confirm_key IS NOT NULL AND confirmed_at IS NOT NULL))"},
			[][]string{{"platform.jobs.jobs_confirmed_shape", "lacks", "'completing'"}}},
		{"the deadline CHECK dropped (a scheduled job nothing could ever expire)",
			[]string{"ALTER TABLE platform.jobs DROP CONSTRAINT jobs_start_needs_a_deadline"},
			[][]string{{"platform.jobs.jobs_start_needs_a_deadline", "missing"}}},
		// ---- platform_0012 and platform_0013 ----
		{"the rows guard dropped (a confirmed job's list could change)",
			[]string{"DROP TRIGGER job_rows_frozen ON platform.job_rows"},
			[][]string{{"platform.job_rows trigger job_rows_frozen", "missing"}}},
		{"the rows guard that does not fire for a DELETE",
			[]string{"DROP TRIGGER job_rows_frozen ON platform.job_rows",
				"CREATE TRIGGER job_rows_frozen BEFORE INSERT OR UPDATE ON platform.job_rows FOR EACH ROW EXECUTE FUNCTION platform.job_rows_frozen()"},
			[][]string{{"job_rows_frozen", "fires as BEFORE INSERT OR UPDATE FOR EACH ROW", "want BEFORE INSERT OR DELETE OR UPDATE"}}},
		{"the rows guard turned into a no-op",
			[]string{"CREATE OR REPLACE FUNCTION platform.job_rows_frozen() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$ BEGIN RETURN NEW; END $$"},
			[][]string{{"job_rows_frozen()", "its body does not mention", "job_rows_frozen"}}},
		{"the results guard dropped (a result for a draft job)",
			[]string{"DROP TRIGGER job_results_after_confirm ON platform.job_results"},
			[][]string{{"platform.job_results trigger job_results_after_confirm", "missing"}}},
		{"the snapshot reachability check dropped (a charge for an unreachable unit could be priced)",
			[]string{"DROP TRIGGER job_unit_prices_only_reachable ON platform.job_unit_prices"},
			[][]string{{"platform.job_unit_prices trigger job_unit_prices_only_reachable", "missing"}}},
		{"the reachability history trigger that fires for every UPDATE of the flag (no WHEN)",
			[]string{"DROP TRIGGER unit_costs_record_reachability ON platform.unit_costs",
				"CREATE TRIGGER unit_costs_record_reachability AFTER UPDATE OF reachable ON platform.unit_costs FOR EACH ROW EXECUTE FUNCTION platform.unit_costs_record_reachability()"},
			[][]string{{"unit_costs_record_reachability", "WHEN clause present = false"}}},
		{"the reachability writer no longer a definer (platform_app could not write the flag at all)",
			[]string{"ALTER FUNCTION platform.set_unit_reachable(text, boolean, text) SECURITY INVOKER"},
			[][]string{{"set_unit_reachable(text, boolean, text)", "is not SECURITY DEFINER"}}},
		{"the reachability writer without a pinned search_path",
			[]string{"ALTER FUNCTION platform.set_unit_reachable(text, boolean, text) RESET search_path"},
			[][]string{{"set_unit_reachable(text, boolean, text)", "does not pin search_path"}}},
		{"the reserve check that counts every unit again (the platform_0005 body)",
			[]string{"CREATE OR REPLACE FUNCTION platform.ledger_check_reserve() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$ BEGIN " +
				"PERFORM 1 FROM platform.job_unit_prices JOIN platform.unit_costs ON true; IF false THEN RAISE EXCEPTION 'x' USING CONSTRAINT = 'reserve_at_the_dearest_price'; END IF; " +
				"IF false THEN RAISE EXCEPTION 'x' USING CONSTRAINT = 'reserve_needs_the_complete_snapshot'; END IF; RETURN NEW; END $$"},
			[][]string{{"ledger_check_reserve()", "its body does not mention", "reachable"}}},
		{"the job_rows state vocabulary without 'excluded'",
			[]string{"ALTER TABLE platform.job_rows DROP CONSTRAINT job_rows_state_vocabulary",
				"ALTER TABLE platform.job_rows ADD CONSTRAINT job_rows_state_vocabulary CHECK (state IN ('pending', 'malformed', 'duplicate', 'suppressed'))"},
			[][]string{{"platform.job_rows.job_rows_state_vocabulary", "lacks", "'excluded'"}}},
		{"the suppressed-row CHECK dropped (a suppressed row could be written without the flag)",
			[]string{"ALTER TABLE platform.job_rows DROP CONSTRAINT job_rows_suppressed_state_has_the_flag"},
			[][]string{{"platform.job_rows.job_rows_suppressed_state_has_the_flag", "missing"}}},
		{"the results guard that no longer derives the flag from the row",
			[]string{"CREATE OR REPLACE FUNCTION platform.job_results_after_confirm() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$ BEGIN " +
				"PERFORM 1 FROM platform.jobs WHERE status IN ('draft', 'previewed'); IF false THEN RAISE EXCEPTION 'x' USING CONSTRAINT = 'jobs_results_after_confirm'; END IF; RETURN NEW; END $$"},
			[][]string{{"job_results_after_confirm()", "its body does not mention", "suppressed"}}},
		{"the charged-result CHECK that does not name the unit",
			[]string{"ALTER TABLE platform.job_results DROP CONSTRAINT job_results_charged_names_its_unit"},
			[][]string{{"platform.job_results.job_results_charged_names_its_unit", "missing"}}},
		{"the event shape CHECK that lets a transition carry no status",
			[]string{"ALTER TABLE platform.job_events DROP CONSTRAINT job_events_transition_shape",
				"ALTER TABLE platform.job_events ADD CONSTRAINT job_events_transition_shape CHECK (kind <> 'transition' OR to_status IS NOT NULL)"},
			[][]string{{"platform.job_events.job_events_transition_shape", "lacks", "from_status IS NOT NULL"}}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := phase2After(t, admin, c.setup)
			if len(got) == 0 {
				t.Fatal("the checker reported nothing for a broken database: it cannot fail")
			}
			for _, want := range c.want {
				if !hasViolation(got, want...) {
					t.Fatalf("no violation mentions %v; the checker said:\n  %s", want, strings.Join(got, "\n  "))
				}
			}
		})
	}

	v, err = phase2CatalogCheck(ctx, admin)
	requireNoViolations(t, v, err, "the database after the rolled-back breakages")
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

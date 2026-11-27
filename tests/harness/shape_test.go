//go:build harness

package harness

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The shape tests. db/module-reads.txt declares what modules 2 and 3 REQUIRE of public.users and public.job_items; db/module-statements.txt lists the statements behind it (held against the
// module source by scripts/check-module-reads.sh). These tests compare the declaration with the catalog of a real migrated database and prove, with deliberately wrong copies, that the comparison
// can fail. The reasons the shape is what it is are in platform_0001_public_tables.up.sql and Docs/phase-1-spec.md (Inventory, F19-F22).

func shapeNewUUID(ctx context.Context, db shapeDB) (string, error) {
	var id string
	err := db.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&id)
	return id, err
}

// TestShape_DeclarationFileParses: the declaration parses, and it still says what the modules' code needs. The module-mandated facts are pinned HERE as well as in the file, so that editing the
// declaration to match a wrong table (campaign_id text, say) is itself a failure. Fails when: a line is malformed; a column the modules need is missing or no longer marked as theirs; a
// module-mandated type or nullability changes; the primary key, the no-foreign-key rule on campaign_id, the user foreign key, the system-user row or the 'pending' probe is deleted.
func TestShape_DeclarationFileParses(t *testing.T) {
	d := shapeLoadReads(t)

	count := map[string]int{}
	for _, c := range d.Columns {
		count[c.Table]++
	}
	if count["public.users"] < 5 || count["public.job_items"] < 9 {
		t.Fatalf("%s declares %d columns of public.users and %d of public.job_items; the floors are 5 and 9 (a declaration that shrank would let the shape drift unseen)", shapeReadsRel, count["public.users"], count["public.job_items"])
	}

	// What module CODE needs (Docs/phase-1-spec.md, Inventory: "Module 2/3 constraints on the platform's two tables"). mod = the modules that must be named in required_by.
	must := []struct {
		table, col, typ string
		notNull         bool
		deflt           *bool
		mod             []string
	}{
		{"public.users", "id", "uuid", true, shapeBool(true), []string{"m2", "m3"}},
		{"public.job_items", "id", "uuid", true, shapeBool(true), []string{"m2", "m3"}}, // module 3's INSERT omits it
		{"public.job_items", "campaign_id", "uuid", true, shapeBool(false), []string{"m2", "m3"}},
		{"public.job_items", "user_id", "uuid", true, shapeBool(false), []string{"m2", "m3"}},
		{"public.job_items", "recipient_email", "text", true, nil, []string{"m2", "m3"}},
		{"public.job_items", "rendered_subject", "text", true, nil, []string{"m2", "m3"}},
		{"public.job_items", "rendered_body", "text", true, nil, []string{"m2", "m3"}},
		{"public.job_items", "reply_to", "text", false, nil, []string{"m2", "m3"}},
		{"public.job_items", "rendered_text", "text", false, nil, []string{"m2", "m3"}},                // the optional plain-text part (Phase 3 close-out B3, C5): both senders freeze it in statement J2
		{"public.job_items", "unsubscribe_url", "text", false, shapeBool(false), []string{"m2", "m3"}}, // the additive carrier of List-Unsubscribe (Phase 5 S3, platform_0017): nullable, no default, written by the platform
		{"public.job_items", "status", "text", true, nil, []string{"m3"}},
	}
	for _, m := range must {
		c, ok := d.column(m.table, m.col)
		if !ok {
			t.Fatalf("%s no longer declares %s.%s, which module code needs", shapeReadsRel, m.table, m.col)
		}
		if c.Type != m.typ || c.NotNull != m.notNull {
			t.Fatalf("%s declares %s.%s as %s notNull=%v; module code requires %s notNull=%v", shapeReadsRel, m.table, m.col, c.Type, c.NotNull, m.typ, m.notNull)
		}
		if m.deflt != nil && c.HasDefault != *m.deflt {
			t.Fatalf("%s declares %s.%s default=%v; module code requires default=%v", shapeReadsRel, m.table, m.col, c.HasDefault, *m.deflt)
		}
		for _, mod := range m.mod {
			if !shapeUsesModule(c.RequiredBy, mod) {
				t.Fatalf("%s: %s.%s is required by %s but its required_by is %q", shapeReadsRel, m.table, m.col, mod, c.RequiredBy)
			}
		}
	}

	hasPK := func(tbl, col string) bool {
		for _, p := range d.PKs {
			if p.Table == tbl && p.Column == col {
				return true
			}
		}
		return false
	}
	if !hasPK("public.users", "id") || !hasPK("public.job_items", "id") {
		t.Fatalf("%s must declare the primary keys public.users(id) and public.job_items(id): 7 + 11 module foreign keys point at them", shapeReadsRel)
	}
	absent := false
	for _, p := range d.FKAbsent {
		absent = absent || (p.Table == "public.job_items" && p.Column == "campaign_id")
	}
	if !absent {
		t.Fatalf("%s must declare fk-absent|public.job_items|campaign_id: module 3's test-send writes the user's id there", shapeReadsRel)
	}
	present := false
	for _, p := range d.FKPresent {
		present = present || (p.Table == "public.job_items" && p.Column == "user_id" && p.Ref == "public.users")
	}
	if !present {
		t.Fatalf("%s must declare fk-present|public.job_items|user_id|public.users", shapeReadsRel)
	}
	system := false
	for _, r := range d.Rows {
		system = system || (r.Table == "public.users" && r.Column == "id" && r.Value == shapeSystemUserID && shapeUsesModule(r.RequiredBy, "m2"))
	}
	if !system {
		t.Fatalf("%s must declare the all-zero system user row as required by m2 (shared pools and accounts answer 409 without it)", shapeReadsRel)
	}
	pending := false
	for _, a := range d.Accepts {
		pending = pending || (a.Column == "status" && a.Value == "pending")
	}
	if !pending {
		t.Fatalf("%s must declare accepts|public.job_items|status|pending: module 3 writes it", shapeReadsRel)
	}
}

func shapeBool(b bool) *bool { return &b }

// TestShape_DeclarationParserRejects is the paired control of the parser: each malformed declaration is refused with the reason, and a minimal valid one is accepted (so the parser is not simply
// refusing everything).
func TestShape_DeclarationParserRejects(t *testing.T) {
	const col = "public.job_items|campaign_id|uuid|NOT NULL|no|m2,m3|note\n"
	good := col + "public.users|id|uuid|NOT NULL|yes|m2,m3|n\npk|public.users|id\nfk-absent|public.job_items|campaign_id\nfk-present|public.job_items|campaign_id|public.users\n" +
		"row|public.users|id|00000000-0000-0000-0000-000000000000|m2|n\n"
	d, err := shapeParseReads(good)
	if err != nil {
		t.Fatalf("a valid declaration was refused: %v", err)
	}
	if len(d.Columns) != 2 || len(d.PKs) != 1 || len(d.FKAbsent) != 1 || len(d.FKPresent) != 1 || len(d.Rows) != 1 {
		t.Fatalf("a valid declaration parsed into %d columns, %d pk, %d fk-absent, %d fk-present, %d rows; want 2, 1, 1, 1, 1", len(d.Columns), len(d.PKs), len(d.FKAbsent), len(d.FKPresent), len(d.Rows))
	}
	if _, err := shapeParseReads("\r\n" + strings.ReplaceAll(good, "\n", "\r\n")); err != nil {
		t.Fatalf("a valid declaration with CRLF line endings was refused: %v", err)
	}

	bad := []struct{ name, text, reason string }{
		{"too few fields", "public.users|id|uuid|NOT NULL|yes\n", "a column line needs"},
		{"an unknown table", "public.orders|id|uuid|NOT NULL|yes|m2|x\n", `unknown table "public.orders"`},
		{"a nullability that is neither", "public.users|id|uuid|MAYBE|yes|m2|x\n", `nullability "MAYBE"`},
		{"a default that is neither", "public.users|id|uuid|NOT NULL|sometimes|m2|x\n", `default "sometimes"`},
		{"an unknown required_by", "public.users|id|uuid|NOT NULL|yes|m4|x\n", `required_by "m4"`},
		{"test-only mixed with a module", "public.users|id|uuid|NOT NULL|yes|m2,test-only|x\n", `required_by "m2,test-only"`},
		{"a column declared twice", "public.users|id|uuid|NOT NULL|yes|m2|x\npublic.users|id|uuid|NOT NULL|yes|m2|x\n", "declared twice"},
		{"a pk on a column nobody declared", "public.users|id|uuid|NOT NULL|yes|m2|x\npk|public.users|zz\n", "names public.users.zz, which has no column line"},
		{"an fk-present without a target", "public.users|id|uuid|NOT NULL|yes|m2|x\nfk-present|public.users|id\n", "fk-present needs"},
		{"a row without a value", "public.users|id|uuid|NOT NULL|yes|m2|x\nrow|public.users|id\n", "row needs"},
		{"accepts on a column the probe does not support", "public.users|id|uuid|NOT NULL|yes|m2|x\naccepts|public.users|id|x|m2\n", "accepts is supported only for public.job_items.status"},
		{"an empty type", "public.users|id||NOT NULL|yes|m2|x\n", "column and type must not be empty"},
	}
	for _, b := range bad {
		b := b
		t.Run(b.name, func(t *testing.T) {
			_, err := shapeParseReads(b.text)
			if err == nil {
				t.Fatalf("the parser accepted %q: it cannot fail", b.text)
			}
			if !strings.Contains(err.Error(), b.reason) {
				t.Fatalf("the parser refused %q for the wrong reason: %v (want %q)", b.text, err, b.reason)
			}
		})
	}

	// the statements declaration has its own parser
	const okStm = "module-dir|m2|coldmail-send-managed\nm2|send_managed_app|U1|a/b.go|SELECT 1\nm2|ref|P1|a/c.go|text\n"
	s, err := shapeParseStatements(okStm)
	if err != nil || len(s.All) != 2 || len(s.SQL("m2")) != 1 {
		t.Fatalf("a valid statements declaration was refused or misread: %v (%d lines, %d SQL)", err, len(s.All), len(s.SQL("m2")))
	}
	badStm := []struct{ name, text, reason string }{
		{"a statement before its module-dir", "m2|send_managed_app|U1|a.go|SELECT 1\n", `module "m2" has no module-dir line before it`},
		{"a role that is not the module's", "module-dir|m2|d\nm2|send_byo_app|U1|a.go|SELECT 1\n", "statements run as"},
		{"a label declared twice", "module-dir|m2|d\nm2|send_managed_app|U1|a.go|SELECT 1\nm2|send_managed_app|U1|b.go|SELECT 2\n", "label U1 is declared twice for module m2"},
		{"a missing SQL field", "module-dir|m2|d\nm2|send_managed_app|U1|a.go\n", "a statement line is"},
		{"no module at all", "# nothing\n", "names no module"},
	}
	for _, b := range badStm {
		b := b
		t.Run("statements: "+b.name, func(t *testing.T) {
			_, err := shapeParseStatements(b.text)
			if err == nil || !strings.Contains(err.Error(), b.reason) {
				t.Fatalf("the statements parser on %q: err=%v, want it to refuse for %q", b.text, err, b.reason)
			}
		})
	}
}

var (
	shapeJiRe      = regexp.MustCompile(`\bji\.([a-z_]+)`)
	shapeWhereRe   = regexp.MustCompile(`(?i)FROM public\.job_items(?: ji)? WHERE ([a-z_.]+) = \$\d+ AND ([a-z_.]+) = \$\d+`)
	shapeSelectRe  = regexp.MustCompile(`(?i)^SELECT ([a-z_]+) FROM public\.job_items`)
	shapeUsersWhRe = regexp.MustCompile(`(?i)FROM public\.users WHERE ([a-z_]+)\s*=`)
)

func shapeWord(sql, word string) bool {
	return regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(word) + `([^A-Za-z0-9_]|$)`).MatchString(sql)
}

// TestShape_DeclarationMatchesStatements ties the two declaration files together, in both directions. (1) Every column marked as required by m2 or m3 is named by a statement of that module.
// (2) Every column a module's statements name (the ji.<col> list, the INSERT column list, the WHERE and SELECT columns of the plain reads) is a declared column marked as required by that module.
// Fails when a statement is added to db/module-statements.txt without declaring its columns (the gate script then holds the statements against the code), or a column keeps its m2/m3 mark after the
// code stopped using it. The floors at the end make a regex that matched nothing a failure.
func TestShape_DeclarationMatchesStatements(t *testing.T) {
	d := shapeLoadReads(t)
	stm := shapeLoadStatements(t)
	for _, mod := range []string{"m2", "m3"} {
		sqls := stm.SQL(mod)
		if len(sqls) == 0 {
			t.Fatalf("%s declares no SQL statement for module %s", shapeStatementsRel, mod)
		}
		// (1) declaration -> statements
		for _, c := range d.Columns {
			if !shapeUsesModule(c.RequiredBy, mod) {
				continue
			}
			named := false
			for _, st := range sqls {
				named = named || shapeWord(st.SQL, c.Name)
			}
			if !named {
				t.Errorf("%s.%s is marked required by %s but no declared statement of %s names it", c.Table, c.Name, mod, mod)
			}
		}
		// (2) statements -> declaration
		derived := map[string]map[string]bool{"public.job_items": {}, "public.users": {}}
		for _, st := range sqls {
			for _, m := range shapeJiRe.FindAllStringSubmatch(st.SQL, -1) {
				derived["public.job_items"][m[1]] = true
			}
			for _, m := range shapeWhereRe.FindAllStringSubmatch(st.SQL, -1) {
				derived["public.job_items"][strings.TrimPrefix(m[1], "ji.")] = true
				derived["public.job_items"][strings.TrimPrefix(m[2], "ji.")] = true
			}
			if m := shapeSelectRe.FindStringSubmatch(st.SQL); m != nil {
				derived["public.job_items"][m[1]] = true
			}
			for _, m := range shapeUsersWhRe.FindAllStringSubmatch(st.SQL, -1) {
				derived["public.users"][m[1]] = true
			}
			if tbl, cols, ok := shapeInsertColumns(st.SQL); ok && tbl == "public.job_items" {
				for _, c := range cols {
					derived[tbl][c] = true
				}
			}
		}
		for tbl, set := range derived {
			names := make([]string, 0, len(set))
			for n := range set {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				c, ok := d.column(tbl, n)
				if !ok {
					t.Errorf("module %s's statements use %s.%s, which %s does not declare", mod, tbl, n, shapeReadsRel)
					continue
				}
				if !shapeUsesModule(c.RequiredBy, mod) {
					t.Errorf("module %s's statements use %s.%s, which %s marks as required by %q only", mod, tbl, n, shapeReadsRel, c.RequiredBy)
				}
			}
		}
		if got := len(derived["public.job_items"]); got < 7 {
			t.Errorf("only %d job_items column(s) were found in module %s's statements, want at least 7 (id, campaign_id, user_id, recipient_email, rendered_subject, rendered_body, reply_to): the instrument matched too little", got, mod)
		}
		if len(derived["public.users"]) < 1 {
			t.Errorf("no public.users column was found in module %s's statements: the instrument matched nothing", mod)
		}
	}
}

// TestShape_CatalogMatchesDeclaration: on a freshly migrated scratch database the catalog matches the declaration exactly: every declared column with its type, nullability and default presence,
// the primary keys, the foreign keys present and absent, the system-user row, 'pending' accepted, and every undeclared job_items column nullable or defaulted. Fails when a migration changes any of them.
func TestShape_CatalogMatchesDeclaration(t *testing.T) {
	_, admin := migratedScratch(t)
	v, err := shapeCheck(testCtx(t), admin, shapeLoadReads(t), "")
	shapeRequireClean(t, v, err, "a freshly migrated scratch database")
}

// TestShape_LiveDatabase runs the same comparison on the LIVE database the harness stack migrated (inside a read-write transaction that is always rolled back: the 'pending' probe inserts a row).
func TestShape_LiveDatabase(t *testing.T) {
	requireLiveMigrated(t)
	tx := shapeRWTx(t, liveAdmin(t))
	v, err := shapeCheck(testCtx(t), tx, shapeLoadReads(t), "")
	shapeRequireClean(t, v, err, "the live database")
}

// TestShape_CheckerCanFail is the paired control of the comparison. It builds copies of the two tables in a scratch SCHEMA inside a transaction that is rolled back, each wrong in exactly one way,
// and requires shapeCheck to report that way by the column and the reason. The first copy is RIGHT and must report nothing, and so are the copies whose only difference is a permitted one (an
// undeclared nullable or defaulted column): a checker that rejected everything would pass the other cases.
func TestShape_CheckerCanFail(t *testing.T) {
	_, admin := migratedScratch(t)
	ctx := testCtx(t)
	decl := shapeLoadReads(t)

	v, err := shapeCheck(ctx, admin, decl, "")
	shapeRequireClean(t, v, err, "the control: the real tables")

	const sch = "shape_ctl"
	cases := []struct {
		name   string
		mutate func(*shapeCopy)
		want   [][]string // each entry: parts that must all appear in one violation; none = the copy must be clean
	}{
		{"a right copy reports nothing", func(c *shapeCopy) {}, nil},
		{"an undeclared NULLABLE column is allowed", func(c *shapeCopy) { c.ExtraLine = "note text," }, nil},
		{"an undeclared NOT NULL column WITH a default is allowed", func(c *shapeCopy) { c.ExtraLine = "priority integer NOT NULL DEFAULT 0," }, nil},
		{"campaign_id of the wrong type", func(c *shapeCopy) { c.CampaignType = "text" },
			[][]string{{"shape_ctl.job_items.campaign_id", "type text", "declared uuid"}}},
		{"campaign_id nullable", func(c *shapeCopy) { c.CampaignNull = "" },
			[][]string{{"job_items.campaign_id", "nullable, declared NOT NULL"}}},
		{"campaign_id with a foreign key", func(c *shapeCopy) { c.CampaignRef = "REFERENCES @S@.users (id)" },
			[][]string{{"job_items.campaign_id", "has a foreign key", "none of any kind"}}},
		{"user_id without its foreign key", func(c *shapeCopy) { c.UserRef = "" },
			[][]string{{"job_items.user_id", "no foreign key to shape_ctl.users"}}},
		{"user_id referencing the wrong table", func(c *shapeCopy) { c.UserRef = "REFERENCES auth.users (id)" },
			[][]string{{"job_items.user_id", "goes to auth.users", "declared shape_ctl.users"}}},
		{"job_items without a primary key", func(c *shapeCopy) { c.ItemsID = "uuid NOT NULL DEFAULT gen_random_uuid()" },
			[][]string{{"shape_ctl.job_items", "no primary key"}}},
		{"users without a primary key", func(c *shapeCopy) { c.UsersID = "uuid UNIQUE NOT NULL DEFAULT gen_random_uuid()" },
			[][]string{{"shape_ctl.users", "no primary key"}}},
		{"job_items.id without a default", func(c *shapeCopy) { c.ItemsID = "uuid PRIMARY KEY" },
			[][]string{{"job_items.id", "has no default", "declared default yes"}}},
		{"job_items.id of the wrong type", func(c *shapeCopy) { c.ItemsID = "text PRIMARY KEY DEFAULT gen_random_uuid()::text" },
			[][]string{{"job_items.id", "type text", "declared uuid"}}},
		{"an undeclared NOT NULL column with no default", func(c *shapeCopy) { c.ExtraLine = "priority integer NOT NULL," },
			[][]string{{"job_items.priority", "NOT NULL without a default", "23502"}}},
		{"reply_to missing", func(c *shapeCopy) { c.ReplyToLine = "" },
			[][]string{{"job_items.reply_to", "declared but missing"}}},
		{"reply_to NOT NULL", func(c *shapeCopy) { c.ReplyToLine = "reply_to text NOT NULL," },
			[][]string{{"job_items.reply_to", "NOT NULL, declared NULL"}}},
		{"a status check that refuses 'pending'", func(c *shapeCopy) { c.StatusCheck = "status <> 'pending'" },
			[][]string{{"job_items.status", "refuses the value 'pending'", "23514"}}},
		{"users.email of another type", func(c *shapeCopy) { c.EmailType = "varchar(120)" },
			[][]string{{"users.email", "character varying(120)", "declared text"}}},
		{"no system user row", func(c *shapeCopy) { c.SystemRow = false },
			[][]string{{"the required row is missing", shapeSystemUserID}}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			tx, err := admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			cp := shapeGoodCopy()
			c.mutate(&cp)
			if err := shapeBuild(ctx, tx, sch, cp); err != nil {
				t.Fatal(err)
			}
			got, err := shapeCheck(ctx, tx, decl, sch)
			if len(c.want) == 0 {
				shapeRequireClean(t, got, err, "the copy")
				return
			}
			for _, w := range c.want {
				shapeRequireReported(t, got, err, w...)
			}
		})
	}

	// Every copy was rolled back: the scratch schema is gone and the real tables are as clean as before.
	var left bool
	if err := admin.QueryRow(ctx, "SELECT to_regnamespace($1) IS NOT NULL", sch).Scan(&left); err != nil || left {
		t.Fatalf("schema %s survived the rolled-back controls (exists=%v, err=%v)", sch, left, err)
	}
	v, err = shapeCheck(ctx, admin, decl, "")
	shapeRequireClean(t, v, err, "the real tables after the controls")
}

// TestShape_UnsuppliedColumnsAreOptional: module 3's test-send runs ONE statement against public.job_items (J4 in db/module-statements.txt) and supplies only the columns it names. Every other
// column must be nullable or defaulted, or the INSERT fails with 23502. The column list is read from the statement, not from the declaration. Control: the same judgement on a copy with an extra
// NOT NULL column reports it, and on the real table with a column removed from the supplied list reports that one.
func TestShape_UnsuppliedColumnsAreOptional(t *testing.T) {
	stm := shapeLoadStatements(t)
	j4 := stm.find(t, "m3", "J4")
	tbl, cols, ok := shapeInsertColumns(j4.SQL)
	if !ok || tbl != "public.job_items" || len(cols) < 4 {
		t.Fatalf("module 3's J4 is not an INSERT INTO public.job_items (<columns>): %q (table %q, %d columns)", j4.SQL, tbl, len(cols))
	}
	_, admin := migratedScratch(t)
	ctx := testCtx(t)

	v, err := shapeUnsupplied(ctx, admin, `"public"."job_items"`, "public.job_items", cols)
	shapeRequireClean(t, v, err, "the real table, judged against module 3's INSERT")

	// control 1: an extra NOT NULL column the INSERT does not supply
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	cp := shapeGoodCopy()
	cp.ExtraLine = "priority integer NOT NULL,"
	if err := shapeBuild(ctx, tx, "shape_ctl", cp); err != nil {
		t.Fatal(err)
	}
	got, err := shapeUnsupplied(ctx, tx, shapeQualified("public.job_items", "shape_ctl"), "shape_ctl.job_items", cols)
	shapeRequireReported(t, got, err, "shape_ctl.job_items.priority", "NOT NULL without a default", "23502")

	// control 2: the same copy with a default on the extra column is fine, so the control above is about the missing default and nothing else
	cp.ExtraLine = "priority integer NOT NULL DEFAULT 0,"
	tx2, err := tx.Begin(ctx) // a savepoint inside the transaction, dropped with it
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(ctx)
	if err := shapeBuild(ctx, tx2, "shape_ctl2", cp); err != nil {
		t.Fatal(err)
	}
	got, err = shapeUnsupplied(ctx, tx2, shapeQualified("public.job_items", "shape_ctl2"), "shape_ctl2.job_items", cols)
	shapeRequireClean(t, got, err, "the copy with a defaulted extra column")

	// control 3: judged against a supplied list that lacks a NOT NULL column, the real table is reported (the function really compares against the list it is given)
	var short []string
	for _, c := range cols {
		if c != "recipient_email" {
			short = append(short, c)
		}
	}
	if len(short) != len(cols)-1 {
		t.Fatalf("module 3's INSERT does not name recipient_email (%v): the control needs a NOT NULL column to leave out", cols)
	}
	got, err = shapeUnsupplied(ctx, admin, `"public"."job_items"`, "public.job_items", short)
	shapeRequireReported(t, got, err, "public.job_items.recipient_email", "23502")
}

// TestShape_Module3InsertWorks runs module 3's EXACT test-send INSERT (J4, read from db/module-statements.txt, parameters bound as strings as the module binds them) against the real table, after
// seeding the user the way the platform does, and requires an id back and the row the module expects: campaign_id = user_id, status 'pending', no reply_to. Controls: an unknown user is refused by
// the user foreign key (23503), a NULL recipient by NOT NULL (23502, naming the column), and a campaign_id that matches nothing at all is accepted (there is no foreign key on it).
func TestShape_Module3InsertWorks(t *testing.T) {
	stm := shapeLoadStatements(t)
	j4 := stm.find(t, "m3", "J4")
	_, admin := migratedScratch(t)
	ctx := testCtx(t)
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	uid, err := shapeSeedUser(ctx, tx, "")
	if err != nil {
		t.Fatal(err)
	}
	const subject, body = "Test message from your account", "<p>This is a test message sent from your own account to confirm it can deliver mail.</p>"
	var id string
	if err := tx.QueryRow(ctx, j4.SQL, uid, "recipient@example.test", subject, body).Scan(&id); err != nil {
		t.Fatalf("module 3's test-send INSERT failed against the platform's public.job_items: %v", err)
	}
	if len(id) != 36 {
		t.Fatalf("RETURNING id gave %q, want a uuid", id)
	}
	var campaign, user, status, email, subj, bod string
	var replyNull, created bool
	if err := tx.QueryRow(ctx, "SELECT campaign_id::text, user_id::text, status, recipient_email, rendered_subject, rendered_body, reply_to IS NULL, created_at IS NOT NULL FROM public.job_items WHERE id = $1", id).
		Scan(&campaign, &user, &status, &email, &subj, &bod, &replyNull, &created); err != nil {
		t.Fatalf("read the inserted row back: %v", err)
	}
	if campaign != uid || user != uid {
		t.Fatalf("the row has campaign_id=%s user_id=%s, want both %s (module 3 binds one parameter to both columns)", campaign, user, uid)
	}
	if status != "pending" || email != "recipient@example.test" || subj != subject || bod != body || !replyNull || !created {
		t.Fatalf("the row is status=%q recipient=%q reply_to NULL=%v created_at set=%v, subject/body match=%v/%v", status, email, replyNull, created, subj == subject, bod == body)
	}

	// control: an unknown user is refused by the platform's user foreign key
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := shapeNewUUID(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	err = sp.QueryRow(ctx, j4.SQL, stranger, "recipient@example.test", subject, body).Scan(&id)
	_ = sp.Rollback(ctx)
	if err == nil || sqlState(err) != "23503" || shapeConstraint(err) != "job_items_user_id_fkey" {
		t.Fatalf("an unknown user: SQLSTATE %q constraint %q (%v), want 23503 job_items_user_id_fkey", sqlState(err), shapeConstraint(err), err)
	}

	// control: a NULL recipient is refused by NOT NULL, naming the column (the modules turn this into an opaque 500)
	sp, err = tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = sp.QueryRow(ctx, j4.SQL, uid, nil, subject, body).Scan(&id)
	_ = sp.Rollback(ctx)
	if err == nil || sqlState(err) != "23502" || shapeColumnOf(err) != "recipient_email" {
		t.Fatalf("a NULL recipient: SQLSTATE %q column %q (%v), want 23502 recipient_email", sqlState(err), shapeColumnOf(err), err)
	}

	// control: campaign_id has no foreign key, so a value that is not a user, a campaign or anything else is accepted
	sp, err = tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Rollback(ctx)
	nobody, err := shapeNewUUID(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Exec(ctx, "INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body) VALUES ($1, $2, 'r@example.test', 's', 'b')", nobody, uid); err != nil {
		t.Fatalf("a campaign_id that matches nothing was refused (%v): campaign_id must carry no foreign key and no check", err)
	}
}

// TestShape_StatusAcceptsPending: the one value module 3 writes to job_items.status is accepted, and is the default. The paired control is a blank status, which the platform refuses with
// job_items_status_not_blank: the probe is not one that accepts anything.
func TestShape_StatusAcceptsPending(t *testing.T) {
	d := shapeLoadReads(t)
	if len(d.Accepts) == 0 {
		t.Fatalf("%s declares no accepts line", shapeReadsRel)
	}
	_, admin := migratedScratch(t)
	ctx := testCtx(t)
	for _, a := range d.Accepts {
		refused, err := shapeProbeStatus(ctx, admin, "", a.Value)
		if err != nil {
			t.Fatalf("the probe could not run: %v", err)
		}
		if refused != nil {
			t.Fatalf("public.job_items refuses status %q (SQLSTATE %s): %v", a.Value, sqlState(refused), refused)
		}
	}
	refused, err := shapeProbeStatus(ctx, admin, "", "   ")
	if err != nil {
		t.Fatalf("the control probe could not run: %v", err)
	}
	if refused == nil || sqlState(refused) != "23514" || shapeConstraint(refused) != "job_items_status_not_blank" {
		t.Fatalf("a blank status: %v (SQLSTATE %q constraint %q), want 23514 job_items_status_not_blank", refused, sqlState(refused), shapeConstraint(refused))
	}

	// the default is 'pending' too
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	uid, err := shapeSeedUser(ctx, tx, "")
	if err != nil {
		t.Fatal(err)
	}
	var def string
	if err := tx.QueryRow(ctx, "INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body) VALUES ($1, $1, 'r@example.test', 's', 'b') RETURNING status", uid).Scan(&def); err != nil || def != "pending" {
		t.Fatalf("the default status is %q (err %v), want pending", def, err)
	}
}

// TestShape_SystemUser: module 2 hard-requires the all-zero uuid user (shared pools and accounts answer 409 without it). It exists on a fresh database, is INACTIVE (it can never act as a person),
// has NO auth link, and is the only user without one. The control reads a regular seeded user through the same query: active and linked, so the query can tell the two apart.
func TestShape_SystemUser(t *testing.T) {
	_, admin := migratedScratch(t)
	ctx := testCtx(t)
	q := "SELECT is_active, auth_user_id IS NULL, email FROM public.users WHERE id = $1"

	var active, noLink bool
	var email string
	if err := admin.QueryRow(ctx, q, shapeSystemUserID).Scan(&active, &noLink, &email); err != nil {
		t.Fatalf("the all-zero system user is missing from public.users: %v", err)
	}
	if active {
		t.Fatal("the system user is ACTIVE: it must never be able to act as a person")
	}
	if !noLink {
		t.Fatal("the system user has an auth link: it has no Supabase login")
	}
	var unlinked int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM public.users WHERE auth_user_id IS NULL").Scan(&unlinked); err != nil || unlinked != 1 {
		t.Fatalf("%d user(s) have no auth link (err %v), want exactly the system user", unlinked, err)
	}

	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	uid, err := shapeSeedUser(ctx, tx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, q, uid).Scan(&active, &noLink, &email); err != nil {
		t.Fatal(err)
	}
	if !active || noLink {
		t.Fatalf("a regular seeded user reads is_active=%v unlinked=%v, want active and linked: the system-user query cannot tell users apart", active, noLink)
	}
}

// TestShape_UsersAuthLink: users_auth_link says every user IS its auth user (id = auth_user_id) or is the one all-zero system row. A user with NO auth link is refused (23514, constraint
// users_auth_link). That is the NULL trap: a CHECK that evaluates to NULL PASSES, and `auth_user_id = id` is NULL when auth_user_id is NULL, so the constraint has to be written with COALESCE.
// Proved three ways: the refusal itself; the constraint's definition contains COALESCE; and a naive twin written WITHOUT it admits exactly that row. A linked user is the positive control.
func TestShape_UsersAuthLink(t *testing.T) {
	_, admin := migratedScratch(t)
	ctx := testCtx(t)
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	authID, err := shapeNewUUID(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO auth.users (id, email) VALUES ($1, 'shape-link@example.test')", authID); err != nil {
		t.Fatalf("seed auth.users: %v", err)
	}

	refused := []struct {
		name, sql string
		args      []any
	}{
		{"no auth link on an ordinary id: the NULL trap", "INSERT INTO public.users (email) VALUES ($1)", []any{"nolink@example.test"}},
		{"an auth link to a different id", "INSERT INTO public.users (id, auth_user_id, email) VALUES (gen_random_uuid(), $1, $2)", []any{authID, "other@example.test"}},
		{"the system id carrying an auth link", "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $2, $3)", []any{shapeSystemUserID, authID, "sys2@example.test"}},
	}
	for _, r := range refused {
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sp.Exec(ctx, r.sql, r.args...)
		_ = sp.Rollback(ctx)
		if err == nil {
			t.Fatalf("%s: the INSERT was ALLOWED (users_auth_link is not enforced)", r.name)
		}
		if sqlState(err) != "23514" || shapeConstraint(err) != "users_auth_link" {
			t.Fatalf("%s: SQLSTATE %q constraint %q (%v), want 23514 users_auth_link", r.name, sqlState(err), shapeConstraint(err), err)
		}
	}

	// positive control: a user that IS its auth user is accepted, so the refusals above are the constraint's doing
	if _, err := tx.Exec(ctx, "INSERT INTO public.users (id, auth_user_id, email) VALUES ($1, $1, 'link@example.test')", authID); err != nil {
		t.Fatalf("a user linked to its own auth id was refused: %v", err)
	}

	// the constraint was written with COALESCE
	var def string
	if err := tx.QueryRow(ctx, "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'users_auth_link' AND conrelid = 'public.users'::regclass").Scan(&def); err != nil {
		t.Fatalf("read users_auth_link: %v", err)
	}
	if !strings.Contains(def, "COALESCE(") {
		t.Fatalf("users_auth_link is %s: it has no COALESCE, so a NULL auth_user_id would make the CHECK evaluate to NULL and PASS", def)
	}

	// the naive twin (the same check without COALESCE) admits the very row the real constraint refused: the trap is real
	for _, s := range []string{
		"CREATE SCHEMA shape_trap",
		"CREATE TABLE shape_trap.t (id uuid, auth_user_id uuid, CONSTRAINT naive CHECK (auth_user_id = id OR (id = '" + shapeSystemUserID + "'::uuid AND auth_user_id IS NULL)))",
	} {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("build the naive twin: %v", err)
		}
	}
	if _, err := tx.Exec(ctx, "INSERT INTO shape_trap.t VALUES (gen_random_uuid(), NULL)"); err != nil {
		t.Fatalf("the naive twin refused a NULL auth_user_id (%v): the NULL trap this test demonstrates did not appear, so the COALESCE proof above proves nothing", err)
	}
}

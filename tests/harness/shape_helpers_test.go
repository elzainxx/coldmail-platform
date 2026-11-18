//go:build harness

package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Shared machinery of shape_test.go, schema_locks_test.go and modules_live_test.go. Every name here starts with `shape` so that it cannot collide with another file's helpers.
//
// Two declarations are read from the repository, never restated in a test:
//   db/module-reads.txt       what modules 2 and 3 REQUIRE of public.users and public.job_items (type, nullability, default, keys, rows)
//   db/module-statements.txt  the statements their code runs against those tables (held against the module source by scripts/check-module-reads.sh)
// The checkers below RETURN violations as sentences, like checkGrants does, and never call t.Error: a paired control can then show that a deliberately wrong database is reported.

const (
	shapeReadsRel      = "db/module-reads.txt"
	shapeStatementsRel = "db/module-statements.txt"
	shapeSystemUserID  = "00000000-0000-0000-0000-000000000000"
	shapeEnvModules    = "HARNESS_MODULES_ROOT"
)

// shapeDB is what the checkers need. *pgx.Conn and pgx.Tx both satisfy it (Begin on a connection starts a transaction, on a transaction it is a savepoint), so a control can run a checker
// inside a transaction it then rolls back.
type shapeDB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func shapeConstraint(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.ConstraintName
	}
	return ""
}

func shapeColumnOf(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.ColumnName
	}
	return ""
}

// shapeHas reports whether ONE violation contains every part.
func shapeHas(v []string, parts ...string) bool {
	for _, line := range v {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func shapeRequireClean(t *testing.T, v []string, err error, where string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: the checker could not run: %v", where, err)
	}
	if len(v) != 0 {
		t.Fatalf("%s: %d violation(s):\n  %s", where, len(v), strings.Join(v, "\n  "))
	}
}

// shapeRequireReported fails unless the checker said something for a deliberately broken database and mentioned every wanted part in one violation: the proof that the checker can fail,
// and that it fails for the reason it names.
func shapeRequireReported(t *testing.T, got []string, err error, want ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("the checker could not run: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("the checker reported nothing for a broken database (wanted a violation mentioning %v): it cannot fail", want)
	}
	if !shapeHas(got, want...) {
		t.Fatalf("no violation mentions all of %v; the checker said:\n  %s", want, strings.Join(got, "\n  "))
	}
}

// shapeRWTx opens a transaction that may write even on liveAdmin's read-only connection (the explicit access mode overrides the session default), and ROLLS IT BACK when the test ends.
// Nothing run in it is ever committed.
func shapeRWTx(t *testing.T, c *pgx.Conn) pgx.Tx {
	t.Helper()
	ctx := testCtx(t)
	tx, err := c.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		t.Fatalf("begin a read-write transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func shapeReadRepoFile(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Join(requireEnv(t, envRepoRoot), filepath.FromSlash(rel))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("cannot read %s: %v", p, err)
	}
	text := strings.ReplaceAll(string(b), "\r", "") // Phase 0 rule 8: every reader strips \r
	if strings.TrimSpace(text) == "" {
		t.Fatalf("%s is empty: the instrument matched nothing", p)
	}
	return text
}

// ---- db/module-reads.txt ----

type shapeColumn struct {
	Table, Name, Type   string
	NotNull, HasDefault bool
	RequiredBy, Note    string
	Line                int
}

type shapeColRef struct {
	Table, Column string
	Line          int
}

type shapeFK struct {
	Table, Column, Ref string
	Line               int
}

type shapeRow struct {
	Table, Column, Value, RequiredBy string
	Line                             int
}

type shapeDecl struct {
	Columns   []shapeColumn
	PKs       []shapeColRef
	FKPresent []shapeFK
	FKAbsent  []shapeColRef
	Rows      []shapeRow
	Accepts   []shapeRow
}

var shapeDeclTables = map[string]bool{"public.users": true, "public.job_items": true}

func shapeValidRequiredBy(s string) bool {
	if s == "test-only" || s == "platform" {
		return true
	}
	if s == "" {
		return false
	}
	for _, p := range strings.Split(s, ",") {
		if p != "m2" && p != "m3" {
			return false
		}
	}
	return true
}

func shapeUsesModule(requiredBy, module string) bool {
	for _, p := range strings.Split(requiredBy, ",") {
		if p == module {
			return true
		}
	}
	return false
}

// shapeParseReads parses db/module-reads.txt. It returns EVERY problem it finds (with the line number), so a malformed declaration fails loudly instead of being half-read.
func shapeParseReads(text string) (shapeDecl, error) {
	var d shapeDecl
	var errs []string
	seen := map[string]bool{}
	for i, raw := range strings.Split(text, "\n") {
		n := i + 1
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "|")
		for k := range f {
			f[k] = strings.TrimSpace(f[k])
		}
		bad := func(format string, a ...any) {
			errs = append(errs, fmt.Sprintf("line %d: ", n)+fmt.Sprintf(format, a...))
		}
		needTable := func(tbl string) bool {
			if !shapeDeclTables[tbl] {
				bad("unknown table %q (the declaration covers public.users and public.job_items)", tbl)
				return false
			}
			return true
		}
		switch f[0] {
		case "pk", "fk-absent":
			if len(f) < 3 || f[1] == "" || f[2] == "" {
				bad("%s needs %s|<table>|<column>", f[0], f[0])
				continue
			}
			if !needTable(f[1]) {
				continue
			}
			ref := shapeColRef{Table: f[1], Column: f[2], Line: n}
			if f[0] == "pk" {
				d.PKs = append(d.PKs, ref)
			} else {
				d.FKAbsent = append(d.FKAbsent, ref)
			}
		case "fk-present":
			if len(f) < 4 || f[1] == "" || f[2] == "" || f[3] == "" || !strings.Contains(f[3], ".") {
				bad("fk-present needs fk-present|<table>|<column>|<schema.referenced table>")
				continue
			}
			if !needTable(f[1]) {
				continue
			}
			d.FKPresent = append(d.FKPresent, shapeFK{Table: f[1], Column: f[2], Ref: f[3], Line: n})
		case "row", "accepts":
			if len(f) < 5 || f[1] == "" || f[2] == "" || f[3] == "" {
				bad("%s needs %s|<table>|<column>|<value>|<required_by>[|note]", f[0], f[0])
				continue
			}
			if !needTable(f[1]) {
				continue
			}
			if !shapeValidRequiredBy(f[4]) {
				bad("required_by %q is not m2, m3, m2,m3, test-only or platform", f[4])
				continue
			}
			r := shapeRow{Table: f[1], Column: f[2], Value: f[3], RequiredBy: f[4], Line: n}
			if f[0] == "row" {
				d.Rows = append(d.Rows, r)
			} else {
				if r.Table != "public.job_items" || r.Column != "status" {
					bad("accepts is supported only for public.job_items.status (the probe inserts a job item), not %s.%s", r.Table, r.Column)
					continue
				}
				d.Accepts = append(d.Accepts, r)
			}
		default:
			// a column line: <table>|<column>|<type>|<NOT NULL|NULL>|<yes|no>|<required_by>[|note]
			if len(f) < 6 {
				bad("a column line needs table|column|type|NOT NULL or NULL|default yes or no|required_by[|note], got %d field(s)", len(f))
				continue
			}
			if !needTable(f[0]) {
				continue
			}
			c := shapeColumn{Table: f[0], Name: f[1], Type: f[2], RequiredBy: f[5], Line: n}
			if len(f) > 6 {
				c.Note = strings.Join(f[6:], "|")
			}
			ok := true
			if c.Name == "" || c.Type == "" {
				bad("column and type must not be empty")
				ok = false
			}
			switch f[3] {
			case "NOT NULL":
				c.NotNull = true
			case "NULL":
			default:
				bad("nullability %q is not NOT NULL or NULL", f[3])
				ok = false
			}
			switch f[4] {
			case "yes":
				c.HasDefault = true
			case "no":
			default:
				bad("default %q is not yes or no", f[4])
				ok = false
			}
			if !shapeValidRequiredBy(c.RequiredBy) {
				bad("required_by %q is not m2, m3, m2,m3, test-only or platform", c.RequiredBy)
				ok = false
			}
			key := c.Table + "." + c.Name
			if seen[key] {
				bad("column %s is declared twice", key)
				ok = false
			}
			seen[key] = true
			if ok {
				d.Columns = append(d.Columns, c)
			}
		}
	}
	// every constraint and row line must name a declared column
	declared := map[string]bool{}
	for _, c := range d.Columns {
		declared[c.Table+"."+c.Name] = true
	}
	need := func(line int, tbl, col, what string) {
		if !declared[tbl+"."+col] {
			errs = append(errs, fmt.Sprintf("line %d: %s names %s.%s, which has no column line", line, what, tbl, col))
		}
	}
	for _, p := range d.PKs {
		need(p.Line, p.Table, p.Column, "pk")
	}
	for _, p := range d.FKPresent {
		need(p.Line, p.Table, p.Column, "fk-present")
	}
	for _, p := range d.FKAbsent {
		need(p.Line, p.Table, p.Column, "fk-absent")
	}
	for _, p := range d.Rows {
		need(p.Line, p.Table, p.Column, "row")
	}
	for _, p := range d.Accepts {
		need(p.Line, p.Table, p.Column, "accepts")
	}
	if len(errs) > 0 {
		return d, errors.New(strings.Join(errs, "\n"))
	}
	return d, nil
}

func (d shapeDecl) tables() []string {
	set := map[string]bool{}
	for _, c := range d.Columns {
		set[c.Table] = true
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func (d shapeDecl) column(tbl, name string) (shapeColumn, bool) {
	for _, c := range d.Columns {
		if c.Table == tbl && c.Name == name {
			return c, true
		}
	}
	return shapeColumn{}, false
}

// ---- db/module-statements.txt ----

type shapeStmt struct {
	Module, Role, Label, File, SQL string
	Line                           int
}

type shapeStmts struct {
	Dirs map[string]string // module id -> checkout directory name under the modules root
	All  []shapeStmt       // every statement line, including the non-SQL references (role "ref")
}

// shapeNormSQL is the normal form of a statement: whitespace runs become one space (the same normal form scripts/check-module-reads.sh compares in).
func shapeNormSQL(s string) string { return strings.Join(strings.Fields(s), " ") }

var shapeModuleRoles = map[string]string{"m2": "send_managed_app", "m3": "send_byo_app"}

func shapeParseStatements(text string) (shapeStmts, error) {
	s := shapeStmts{Dirs: map[string]string{}}
	var errs []string
	seen := map[string]bool{}
	for i, raw := range strings.Split(text, "\n") {
		n := i + 1
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.SplitN(line, "|", 5)
		for k := 0; k < len(f) && k < 4; k++ {
			f[k] = strings.TrimSpace(f[k])
		}
		bad := func(format string, a ...any) {
			errs = append(errs, fmt.Sprintf("line %d: ", n)+fmt.Sprintf(format, a...))
		}
		if f[0] == "module-dir" {
			f = strings.Split(line, "|")
			if len(f) != 3 || strings.TrimSpace(f[1]) == "" || strings.TrimSpace(f[2]) == "" {
				bad("a module-dir line is module-dir|<id>|<directory>")
				continue
			}
			s.Dirs[strings.TrimSpace(f[1])] = strings.TrimSpace(f[2])
			continue
		}
		if len(f) != 5 || f[0] == "" || f[1] == "" || f[2] == "" || f[3] == "" || strings.TrimSpace(f[4]) == "" {
			bad("a statement line is <module>|<role>|<label>|<file>|<SQL> with all five fields filled")
			continue
		}
		st := shapeStmt{Module: f[0], Role: f[1], Label: f[2], File: f[3], SQL: shapeNormSQL(f[4]), Line: n}
		if _, ok := s.Dirs[st.Module]; !ok {
			bad("module %q has no module-dir line before it", st.Module)
			continue
		}
		if st.Role != "ref" && st.Role != shapeModuleRoles[st.Module] {
			bad("module %s statements run as %q, not %q", st.Module, shapeModuleRoles[st.Module], st.Role)
			continue
		}
		key := st.Module + "/" + st.Label
		if seen[key] {
			bad("label %s is declared twice for module %s", st.Label, st.Module)
			continue
		}
		seen[key] = true
		s.All = append(s.All, st)
	}
	if len(s.Dirs) == 0 {
		errs = append(errs, "no module-dir line: the declaration names no module")
	}
	if len(errs) > 0 {
		return s, errors.New(strings.Join(errs, "\n"))
	}
	return s, nil
}

// SQL returns the executable statements of one module (the non-SQL references are left out).
func (s shapeStmts) SQL(module string) []shapeStmt {
	var out []shapeStmt
	for _, st := range s.All {
		if st.Module == module && st.Role != "ref" {
			out = append(out, st)
		}
	}
	return out
}

func (s shapeStmts) find(t *testing.T, module, label string) shapeStmt {
	t.Helper()
	for _, st := range s.All {
		if st.Module == module && st.Label == label && st.Role != "ref" {
			return st
		}
	}
	t.Fatalf("%s declares no SQL statement %s for module %s", shapeStatementsRel, label, module)
	return shapeStmt{}
}

func shapeLoadReads(t *testing.T) shapeDecl {
	t.Helper()
	d, err := shapeParseReads(shapeReadRepoFile(t, shapeReadsRel))
	if err != nil {
		t.Fatalf("%s does not parse:\n%v", shapeReadsRel, err)
	}
	return d
}

func shapeLoadStatements(t *testing.T) shapeStmts {
	t.Helper()
	s, err := shapeParseStatements(shapeReadRepoFile(t, shapeStatementsRel))
	if err != nil {
		t.Fatalf("%s does not parse:\n%v", shapeStatementsRel, err)
	}
	return s
}

var shapeInsertRe = regexp.MustCompile(`(?i)^INSERT INTO ([a-z_.]+) \(([^)]*)\)`)

// shapeInsertColumns returns the table and the explicit column list of an INSERT statement.
func shapeInsertColumns(sql string) (string, []string, bool) {
	m := shapeInsertRe.FindStringSubmatch(sql)
	if m == nil {
		return "", nil, false
	}
	var cols []string
	for _, c := range strings.Split(m[2], ",") {
		cols = append(cols, strings.TrimSpace(c))
	}
	return m[1], cols, true
}

// ---- the catalog ----

type shapeCatalogCol struct {
	Type    string
	NotNull bool
	Default bool // a default expression, an identity or a generated column: an INSERT may omit it
}

// shapeTarget maps a declared table (public.x) to the table a checker looks at: the same name in schema `schema` when one is given (the paired controls build wrong copies in a scratch schema).
func shapeTarget(tbl, schema string) (string, string) {
	sch, rel, _ := strings.Cut(tbl, ".")
	if schema != "" && sch == "public" {
		sch = schema
	}
	return sch, rel
}

func shapeQualified(tbl, schema string) string {
	sch, rel := shapeTarget(tbl, schema)
	return pgx.Identifier{sch, rel}.Sanitize()
}

func shapeLabel(tbl, schema string) string {
	sch, rel := shapeTarget(tbl, schema)
	return sch + "." + rel
}

func shapeCatalogColumns(ctx context.Context, db shapeDB, qualified string) (map[string]shapeCatalogCol, []string, bool, error) {
	var exists bool
	if err := db.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", qualified).Scan(&exists); err != nil {
		return nil, nil, false, err
	}
	if !exists {
		return nil, nil, false, nil
	}
	rows, err := db.Query(ctx, `
		SELECT a.attname::text, format_type(a.atttypid, a.atttypmod), a.attnotnull, (a.atthasdef OR a.attidentity <> '' OR a.attgenerated <> '')
		FROM pg_attribute a
		WHERE a.attrelid = to_regclass($1) AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, qualified)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	cols := map[string]shapeCatalogCol{}
	var order []string
	for rows.Next() {
		var name string
		var c shapeCatalogCol
		if err := rows.Scan(&name, &c.Type, &c.NotNull, &c.Default); err != nil {
			return nil, nil, false, err
		}
		cols[name] = c
		order = append(order, name)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}
	return cols, order, true, nil
}

func shapeCatalogPK(ctx context.Context, db shapeDB, qualified string) ([]string, error) {
	var pk []string
	err := db.QueryRow(ctx, `
		SELECT COALESCE(array_agg(a.attname::text ORDER BY k.ord), '{}'::text[])
		FROM pg_constraint c
		CROSS JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord)
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		WHERE c.conrelid = to_regclass($1) AND c.contype = 'p'`, qualified).Scan(&pk)
	return pk, err
}

type shapeCatalogFK struct {
	Name               string
	FromThis, ToThis   bool
	FromTable, ToTable string
	Cols, RefCols      []string
}

// shapeCatalogFKs lists every foreign key that has this table on either side.
func shapeCatalogFKs(ctx context.Context, db shapeDB, qualified string) ([]shapeCatalogFK, error) {
	rows, err := db.Query(ctx, `
		SELECT c.conname::text,
		       (c.conrelid = to_regclass($1)), (c.confrelid = to_regclass($1)),
		       rn.nspname || '.' || rc.relname, fn.nspname || '.' || fc.relname,
		       COALESCE((SELECT array_agg(a.attname::text ORDER BY k.ord) FROM unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord)
		                  JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum), '{}'::text[]),
		       COALESCE((SELECT array_agg(a.attname::text ORDER BY k.ord) FROM unnest(c.confkey) WITH ORDINALITY AS k(attnum, ord)
		                  JOIN pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = k.attnum), '{}'::text[])
		FROM pg_constraint c
		JOIN pg_class rc ON rc.oid = c.conrelid JOIN pg_namespace rn ON rn.oid = rc.relnamespace
		JOIN pg_class fc ON fc.oid = c.confrelid JOIN pg_namespace fn ON fn.oid = fc.relnamespace
		WHERE c.contype = 'f' AND (c.conrelid = to_regclass($1) OR c.confrelid = to_regclass($1))
		ORDER BY c.conname`, qualified)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []shapeCatalogFK
	for rows.Next() {
		var f shapeCatalogFK
		if err := rows.Scan(&f.Name, &f.FromThis, &f.ToThis, &f.FromTable, &f.ToTable, &f.Cols, &f.RefCols); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func shapeContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// shapeInsertedByModules are the tables module CODE inserts into (module 3's test-send). On those, every column the declaration does not name must be nullable or defaulted.
var shapeInsertedByModules = map[string]bool{"public.job_items": true}

// shapeCheck compares the declaration with the catalog and returns every difference as a sentence. schema "" checks the declared schema (public); a schema name checks the same tables in that
// schema instead (the paired controls). It never calls t.Error.
//
// What it checks: each declared column exists with the declared type (format_type), nullability and default presence; the primary keys; each foreign key declared present (to the declared
// table) or absent (none of ANY kind on that column, either direction); each declared row exists; each `accepts` value is accepted by a real INSERT (inside a rolled-back savepoint); and on the
// tables module code inserts into, every undeclared column is nullable or defaulted. NOT checked: CHECK constraints other than through the accepts probe, indexes, privileges (modules_live_test.go),
// triggers and row-level security (the platform adds none; the grants test and the live statement run would show their effect).
func shapeCheck(ctx context.Context, db shapeDB, d shapeDecl, schema string) ([]string, error) {
	var v []string
	if len(d.Columns) == 0 {
		return []string{"the declaration has no column line: the instrument matched nothing"}, nil
	}
	catalog := map[string]map[string]shapeCatalogCol{}
	missing := map[string]bool{}
	for _, tbl := range d.tables() {
		cols, order, exists, err := shapeCatalogColumns(ctx, db, shapeQualified(tbl, schema))
		if err != nil {
			return nil, err
		}
		if !exists {
			v = append(v, fmt.Sprintf("%s: the table does not exist", shapeLabel(tbl, schema)))
			missing[tbl] = true
			continue
		}
		catalog[tbl] = cols
		declared := map[string]bool{}
		for _, c := range d.Columns {
			if c.Table != tbl {
				continue
			}
			declared[c.Name] = true
			where := fmt.Sprintf("%s.%s", shapeLabel(tbl, schema), c.Name)
			got, ok := cols[c.Name]
			if !ok {
				v = append(v, fmt.Sprintf("%s: declared but missing from the table (required by %s)", where, c.RequiredBy))
				continue
			}
			if got.Type != c.Type {
				v = append(v, fmt.Sprintf("%s: type %s, declared %s (required by %s)", where, got.Type, c.Type, c.RequiredBy))
			}
			switch {
			case c.NotNull && !got.NotNull:
				v = append(v, fmt.Sprintf("%s: nullable, declared NOT NULL (required by %s)", where, c.RequiredBy))
			case !c.NotNull && got.NotNull:
				v = append(v, fmt.Sprintf("%s: NOT NULL, declared NULL (required by %s)", where, c.RequiredBy))
			}
			switch {
			case c.HasDefault && !got.Default:
				v = append(v, fmt.Sprintf("%s: has no default, declared default yes (required by %s)", where, c.RequiredBy))
			case !c.HasDefault && got.Default:
				v = append(v, fmt.Sprintf("%s: has a default, declared default no (required by %s)", where, c.RequiredBy))
			}
		}
		if shapeInsertedByModules[tbl] {
			for _, name := range order {
				if declared[name] {
					continue
				}
				if c := cols[name]; c.NotNull && !c.Default {
					v = append(v, fmt.Sprintf("%s.%s: not declared in %s and NOT NULL without a default: module 3's six-column INSERT omits it and would fail with 23502", shapeLabel(tbl, schema), name, shapeReadsRel))
				}
			}
		}
	}

	for _, p := range d.PKs {
		if missing[p.Table] {
			continue
		}
		pk, err := shapeCatalogPK(ctx, db, shapeQualified(p.Table, schema))
		if err != nil {
			return nil, err
		}
		switch {
		case len(pk) == 0:
			v = append(v, fmt.Sprintf("%s: no primary key, declared (%s)", shapeLabel(p.Table, schema), p.Column))
		case len(pk) != 1 || pk[0] != p.Column:
			v = append(v, fmt.Sprintf("%s: primary key is (%s), declared (%s)", shapeLabel(p.Table, schema), strings.Join(pk, ", "), p.Column))
		}
	}

	fks := map[string][]shapeCatalogFK{}
	for _, tbl := range d.tables() {
		if missing[tbl] {
			continue
		}
		list, err := shapeCatalogFKs(ctx, db, shapeQualified(tbl, schema))
		if err != nil {
			return nil, err
		}
		fks[tbl] = list
	}
	for _, p := range d.FKPresent {
		if missing[p.Table] {
			continue
		}
		refSch, refRel := shapeTarget(p.Ref, schema)
		wantRef := refSch + "." + refRel
		found, others := false, []string{}
		for _, f := range fks[p.Table] {
			if f.FromThis && len(f.Cols) == 1 && f.Cols[0] == p.Column {
				if f.ToTable == wantRef {
					found = true
				} else {
					others = append(others, f.ToTable)
				}
			}
		}
		if !found {
			where := fmt.Sprintf("%s.%s", shapeLabel(p.Table, schema), p.Column)
			if len(others) > 0 {
				v = append(v, fmt.Sprintf("%s: the foreign key goes to %s, declared %s", where, strings.Join(others, ", "), wantRef))
			} else {
				v = append(v, fmt.Sprintf("%s: no foreign key to %s", where, wantRef))
			}
		}
	}
	for _, p := range d.FKAbsent {
		if missing[p.Table] {
			continue
		}
		for _, f := range fks[p.Table] {
			if (f.FromThis && shapeContains(f.Cols, p.Column)) || (f.ToThis && shapeContains(f.RefCols, p.Column)) {
				dir := fmt.Sprintf("from %s to %s", f.FromTable, f.ToTable)
				v = append(v, fmt.Sprintf("%s.%s: has a foreign key %s (%s): it must have none of any kind (module 3's test-send writes the user's id there, which no foreign key can accept)", shapeLabel(p.Table, schema), p.Column, f.Name, dir))
			}
		}
	}

	for _, r := range d.Rows {
		if missing[r.Table] {
			continue
		}
		var n int
		q := "SELECT count(*) FROM " + shapeQualified(r.Table, schema) + " WHERE " + pgx.Identifier{r.Column}.Sanitize() + "::text = $1"
		if err := db.QueryRow(ctx, q, r.Value).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			v = append(v, fmt.Sprintf("%s.%s = %s: the required row is missing (required by %s)", shapeLabel(r.Table, schema), r.Column, r.Value, r.RequiredBy))
		}
	}

	for _, a := range d.Accepts {
		if missing[a.Table] {
			continue
		}
		refused, err := shapeProbeStatus(ctx, db, schema, a.Value)
		if err != nil {
			return nil, err
		}
		if refused != nil {
			v = append(v, fmt.Sprintf("%s.%s: refuses the value '%s' (SQLSTATE %s: %v): required by %s", shapeLabel(a.Table, schema), a.Column, a.Value, sqlState(refused), refused, a.RequiredBy))
		}
	}
	return v, nil
}

// shapeSeedUser inserts a user the way the platform does (an auth.users row, then a public.users row linked to it) and returns its id. The caller owns the transaction.
func shapeSeedUser(ctx context.Context, q shapeDB, schema string) (string, error) {
	var id string
	if err := q.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		return "", err
	}
	email := "shape-" + id + "@example.test"
	if _, err := q.Exec(ctx, "INSERT INTO auth.users (id, email) VALUES ($1, $2)", id, email); err != nil {
		return "", fmt.Errorf("seed auth.users: %w", err)
	}
	sch := "public"
	if schema != "" {
		sch = schema
	}
	if _, err := q.Exec(ctx, "INSERT INTO "+pgx.Identifier{sch, "users"}.Sanitize()+" (id, auth_user_id, email) VALUES ($1, $1, $2)", id, email); err != nil {
		return "", fmt.Errorf("seed %s.users: %w", sch, err)
	}
	return id, nil
}

// shapeProbeStatus inserts one job item carrying `status` inside a savepoint that is always rolled back. It returns (the database's refusal, nil) when the INSERT is refused, and (nil, err) only
// when the probe itself could not be set up.
func shapeProbeStatus(ctx context.Context, db shapeDB, schema, status string) (refused error, err error) {
	sp, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer sp.Rollback(ctx)
	uid, err := shapeSeedUser(ctx, sp, schema)
	if err != nil {
		return nil, err
	}
	sch := "public"
	if schema != "" {
		sch = schema
	}
	_, ierr := sp.Exec(ctx, "INSERT INTO "+pgx.Identifier{sch, "job_items"}.Sanitize()+
		" (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status) VALUES (gen_random_uuid(), $1, 'shape@example.test', 'subject', 'body', $2)", uid, status)
	return ierr, nil
}

// shapeUnsupplied returns a violation for every column of the table that a statement supplying only `supplied` would leave to the database and that has neither a default nor NULL allowed.
func shapeUnsupplied(ctx context.Context, db shapeDB, qualified, label string, supplied []string) ([]string, error) {
	cols, order, exists, err := shapeCatalogColumns(ctx, db, qualified)
	if err != nil {
		return nil, err
	}
	if !exists {
		return []string{label + ": the table does not exist"}, nil
	}
	var v []string
	for _, name := range order {
		if shapeContains(supplied, name) {
			continue
		}
		if c := cols[name]; c.NotNull && !c.Default {
			v = append(v, fmt.Sprintf("%s.%s: NOT NULL without a default, but the statement supplies only (%s): it would fail with SQLSTATE 23502", label, name, strings.Join(supplied, ", ")))
		}
	}
	return v, nil
}

// ---- deliberately wrong copies of the two tables (the paired controls) ----

// shapeCopy describes a copy of public.users / public.job_items in a scratch schema. The zero-value fields of shapeGoodCopy() give a copy that satisfies the declaration; each control changes
// one field. A field is a SQL fragment, so a control can make the copy wrong in exactly one way.
type shapeCopy struct {
	UsersID      string // the id column's definition after its name
	EmailType    string
	SystemRow    bool
	ItemsID      string
	CampaignType string
	CampaignNull string
	CampaignRef  string // an inline REFERENCES clause for campaign_id, "" for none
	UserRef      string // an inline REFERENCES clause for user_id; the token @S@ stands for the scratch schema
	ReplyToLine  string // the whole reply_to column line including its comma, "" to drop the column
	ExtraLine    string // an extra column line including its comma
	StatusCheck  string // the CHECK expression on status
}

func shapeGoodCopy() shapeCopy {
	return shapeCopy{
		UsersID:      "uuid PRIMARY KEY DEFAULT gen_random_uuid()",
		EmailType:    "text",
		SystemRow:    true,
		ItemsID:      "uuid PRIMARY KEY DEFAULT gen_random_uuid()",
		CampaignType: "uuid",
		CampaignNull: "NOT NULL",
		UserRef:      "REFERENCES @S@.users (id)",
		ReplyToLine:  "reply_to text,",
		StatusCheck:  "btrim(status) <> ''",
	}
}

// shapeBuild creates the copy in `schema` inside the caller's transaction (which the caller rolls back).
func shapeBuild(ctx context.Context, tx shapeDB, schema string, c shapeCopy) error {
	sub := func(s string) string { return strings.ReplaceAll(s, "@S@", pgx.Identifier{schema}.Sanitize()) }
	stmts := []string{
		"CREATE SCHEMA " + pgx.Identifier{schema}.Sanitize(),
		sub(fmt.Sprintf(`CREATE TABLE @S@.users (
			id %s,
			auth_user_id uuid UNIQUE REFERENCES auth.users (id),
			email %s NOT NULL,
			is_active boolean NOT NULL DEFAULT true,
			created_at timestamptz NOT NULL DEFAULT now(),
			CONSTRAINT users_auth_link CHECK (COALESCE(auth_user_id = id, false) OR (id = '%s'::uuid AND auth_user_id IS NULL))
		)`, c.UsersID, c.EmailType, shapeSystemUserID)),
	}
	if c.SystemRow {
		stmts = append(stmts, sub(fmt.Sprintf("INSERT INTO @S@.users (id, auth_user_id, email, is_active) VALUES ('%s', NULL, 'system@platform.invalid', false)", shapeSystemUserID)))
	}
	stmts = append(stmts, sub(fmt.Sprintf(`CREATE TABLE @S@.job_items (
			id %s,
			campaign_id %s %s %s,
			user_id uuid NOT NULL %s,
			recipient_email text NOT NULL,
			rendered_subject text NOT NULL,
			rendered_body text NOT NULL,
			rendered_text text,
			%s
			status text NOT NULL DEFAULT 'pending',
			%s
			created_at timestamptz NOT NULL DEFAULT now(),
			CONSTRAINT job_items_status_not_blank CHECK (%s)
		)`, c.ItemsID, c.CampaignType, c.CampaignNull, c.CampaignRef, c.UserRef, c.ReplyToLine, c.ExtraLine, c.StatusCheck)))
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("build the scratch copy: %w (statement: %s)", err, strings.Join(strings.Fields(s), " "))
		}
	}
	return nil
}

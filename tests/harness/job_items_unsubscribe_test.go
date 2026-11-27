//go:build harness

package harness

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// platform_0017: public.job_items.unsubscribe_url and its CHECK job_items_unsubscribe_url_shape, on a freshly migrated scratch database. Every refusal names its constraint (a refusal for any other reason
// fails the test), and every refusal has its accepted twin: the NULL case (rule 13: a CHECK over a nullable column must let NULL through by its own clause), the shortest and the longest good link, and a
// link that differs from a refused one in the one thing that was wrong with it.

const (
	linkInsert = "INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, unsubscribe_url) VALUES ($1, $1, 'r@example.com', 's', '<p>b</p>', $2)"
	linkUpdate = "UPDATE public.job_items SET unsubscribe_url = $2 WHERE user_id = $1"
	linkCheck  = "job_items_unsubscribe_url_shape"
)

func linkTx(t *testing.T) pgx.Tx {
	t.Helper()
	_, admin := migratedScratch(t)
	tx, err := admin.Begin(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(testCtx(t)) })
	ledgerSeed(t, tx)
	return tx
}

func TestJobItemsUnsubscribeURL_TheColumnIsNullableWithNoDefaultAndHasNoColumnGrantOfItsOwn(t *testing.T) {
	_, admin := migratedScratch(t)
	if n := ledgerInt64(t, admin, "SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'job_items' AND column_name = 'unsubscribe_url' AND data_type = 'text' AND is_nullable = 'YES' AND column_default IS NULL"); n != 1 {
		t.Fatalf("public.job_items.unsubscribe_url is not a nullable text column with no default (found %d)", n)
	}
	// the privileges are the table's: no column-level ACL, so the table-level SELECT of the module roles (granted by their migrations) and the platform roles' grants cover the column
	if n := ledgerInt64(t, admin, "SELECT count(*) FROM pg_attribute WHERE attrelid = 'public.job_items'::regclass AND attname = 'unsubscribe_url' AND attacl IS NULL AND NOT attisdropped"); n != 1 {
		t.Fatalf("the column has a column-level ACL of its own (or is missing): %d", n)
	}
	for _, c := range []struct {
		role, priv string
		want       bool
	}{
		{"platform_app", "SELECT", true}, {"platform_app", "INSERT", true}, {"platform_app", "UPDATE", true}, {"platform_admin", "SELECT", true}, {"platform_admin", "UPDATE", false}, {"platform_admin", "INSERT", false},
	} {
		if got := ledgerBool(t, admin, "SELECT has_column_privilege($1::text, 'public.job_items', 'unsubscribe_url', $2)", c.role, c.priv); got != c.want {
			t.Errorf("%s %s on the column = %v, want %v", c.role, c.priv, got, c.want)
		}
	}
	// the CHECK is on the table and is the only constraint that names the column
	if n := ledgerInt64(t, admin, "SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.job_items'::regclass AND contype = 'c' AND conname = $1 AND pg_get_constraintdef(oid) LIKE '%unsubscribe_url IS NULL%'", linkCheck); n != 1 {
		t.Fatalf("the CHECK %s is missing or does not say IS NULL first (rule 13): %d", linkCheck, n)
	}
}

func TestJobItemsUnsubscribeURL_TheCheckAcceptsNULLAndGoodLinksAndNamesWhatItRefuses(t *testing.T) {
	tx := linkTx(t)
	a := ledgerUserA
	why := ledgerWhy{Constraint: linkCheck, Table: "job_items"}
	longest := "https://" + strings.Repeat("a", 2000-len("https://"))

	// the accepted twins first: each shows the statement is otherwise good
	for name, link := range map[string]any{
		"NULL (rule 13: a CHECK over a nullable column lets NULL through by its own clause)": nil,
		"a link the platform makes":                  "https://platform.example.test/u/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCdEfGhIjKlMn",
		"the shortest: the scheme and one character": "https://a",
		"a query":                           "https://platform.example.test/u/x?y=1&z=2",
		"a port and a path prefix":          "https://platform.example.test:8443/mail/u/x",
		"exactly 2000 bytes":                longest,
		"2000 bytes of two-byte characters": "https://" + strings.Repeat("é", (2000-len("https://"))/2),
		"a non-ASCII character (allowed by the rule as written)": "https://platform.example.test/u/é",
	} {
		ledgerExpectOK(t, tx, name, linkInsert, a, link)
	}

	for name, link := range map[string]string{
		"a space":                              "https://platform.example.test/u/a b",
		"a leading space":                      " https://platform.example.test/u/x",
		"a trailing space":                     "https://platform.example.test/u/x ",
		"a tab":                                "https://platform.example.test/u/a\tb",
		"a newline inside":                     "https://platform.example.test/u/a\nb",
		"a trailing newline":                   "https://platform.example.test/u/x\n",
		"a carriage return":                    "https://platform.example.test/u/x\r",
		"a form feed":                          "https://platform.example.test/u/a\fb",
		"an opening angle bracket":             "https://platform.example.test/u/<x",
		"a closing angle bracket":              "https://platform.example.test/u/x>",
		"both, as in a header":                 "<https://platform.example.test/u/x>",
		"http, not https":                      "http://platform.example.test/u/x",
		"an upper-case scheme":                 "HTTPS://platform.example.test/u/x",
		"another scheme":                       "ftp://platform.example.test/u/x",
		"no scheme":                            "platform.example.test/u/x",
		"javascript":                           "javascript:alert(1)",
		"the scheme alone":                     "https://",
		"the empty string (which is not NULL)": "",
		"2001 bytes":                           longest + "a",
		"2002 bytes of two-byte characters":    "https://" + strings.Repeat("é", (2000-len("https://"))/2+1),
		"DEL":                                  "https://platform.example.test/u/\x7f",
		"a control character (SOH)":            "https://platform.example.test/u/\x01",
		"a control character (ESC)":            "https://platform.example.test/u/\x1b",
	} {
		ledgerExpectRefused(t, tx, name, "23514", why, linkInsert, a, link)
	}

	// the same rules on an UPDATE (the retention of Phase 5 clears the column with an UPDATE: NULL must pass, anything else is judged like an insert)
	ledgerExpectOK(t, tx, "a row with a good link", linkInsert, a, "https://platform.example.test/u/x")
	ledgerExpectOK(t, tx, "an UPDATE to NULL", linkUpdate, a, nil)
	ledgerExpectOK(t, tx, "an UPDATE to another good link", linkUpdate, a, "https://platform.example.test/u/y")
	ledgerExpectRefused(t, tx, "an UPDATE to a link with a space", "23514", why, linkUpdate, a, "https://platform.example.test/u/a b")
	ledgerExpectRefused(t, tx, "an UPDATE to http", "23514", why, linkUpdate, a, "http://platform.example.test/u/x")
}

func TestJobItemsUnsubscribeURL_ModuleThreesSixColumnTestSendInsertStillWorksAndLeavesTheColumnNULL(t *testing.T) {
	tx := linkTx(t)
	a := ledgerUserA
	const six = "INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status) VALUES ($1, $1, 'test-send@example.com', 's', '<p>b</p>', 'pending') RETURNING id"
	var id string
	if err := tx.QueryRow(testCtx(t), six, a).Scan(&id); err != nil {
		t.Fatalf("module 3's six-column test-send INSERT (statement J4) failed: %v", err)
	}
	var link *string
	if err := tx.QueryRow(testCtx(t), "SELECT unsubscribe_url FROM public.job_items WHERE id = $1", id).Scan(&link); err != nil || link != nil {
		t.Fatalf("the test-send row's link is %v (%v), want NULL: a test send carries no unsubscribe header", link, err)
	}
	// the control: the same table refuses a row that supplies a bad link, so the insert above passed because the column is optional and not because nothing is checked
	ledgerExpectRefused(t, tx, "the six columns and a bad link", "23514", ledgerWhy{Constraint: linkCheck},
		"INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status, unsubscribe_url) VALUES ($1, $1, 'x@example.com', 's', 'b', 'pending', 'http://x')", a)
}

func TestJobItemsUnsubscribeURL_TheApplicationRoleWritesItAndTheAdminRoleOnlyReadsIt(t *testing.T) {
	tx := linkTx(t)
	a := ledgerUserA
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		if _, err := sp.Exec(testCtx(t), "SET LOCAL ROLE platform_app"); err != nil {
			t.Fatal(err)
		}
		var who string
		if err := sp.QueryRow(testCtx(t), "SELECT current_user").Scan(&who); err != nil || who != "platform_app" {
			t.Fatalf("the role switch did not bite: current_user = %q (%v)", who, err)
		}
		ledgerExpectOK(t, sp, "platform_app inserts a row with a link", linkInsert, a, "https://platform.example.test/u/x")
		ledgerExpectOK(t, sp, "platform_app clears it", linkUpdate, a, nil)
		ledgerExpectRefused(t, sp, "platform_app writes a bad link", "23514", ledgerWhy{Constraint: linkCheck}, linkUpdate, a, "ftp://x")
	})
	ledgerTrial(t, tx, func(sp pgx.Tx) {
		ledgerMust(t, sp, "INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, unsubscribe_url) VALUES ($1, $1, 'r@example.com', 's', 'b', 'https://platform.example.test/u/x')", a)
		if _, err := sp.Exec(testCtx(t), "SET LOCAL ROLE platform_admin"); err != nil {
			t.Fatal(err)
		}
		var link string
		if err := sp.QueryRow(testCtx(t), "SELECT unsubscribe_url FROM public.job_items WHERE user_id = $1", a).Scan(&link); err != nil || link == "" {
			t.Fatalf("platform_admin cannot read the column: %q, %v", link, err)
		}
		ledgerExpectRefused(t, sp, "platform_admin writes the column", "42501", ledgerWhy{Message: "permission denied"}, linkUpdate, a, "https://platform.example.test/u/y")
	})
}

//go:build harness

package harness

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/unsubscribe"
)

// Phase 5 S3, the job store and the unsubscribe link, against a real database (db-tests runs them on the runner): the link written into public.job_items.unsubscribe_url for every item of a send job
// (and NULL when the store has no maker), the same link on the row the renderer sees, ItemOwner over real rows, and the refusal to confirm a send job in a store that cannot make a link. The column's
// own CHECK is in job_items_unsubscribe_test.go. The harness stores are built WITH a maker by default (harnessUnsubscribeOption: newJeEnv and newJbsEnv), because a send job cannot be confirmed without
// one; a test that wants the other state says jobs.WithUnsubscribe(nil).

const harnessPublicBase = "https://platform.example.test"

// harnessUnsubSigner is the signer of every harness store: a secret built at run time from a repeated word (it protects nothing: the databases are scratch databases of one run).
var harnessUnsubSigner = func() *unsubscribe.Signer {
	s, err := unsubscribe.NewSigner([]byte(strings.Repeat("harness-unsubscribe-", 3)))
	if err != nil {
		panic(err)
	}
	return s
}()

// harnessUnsubscribeOption is the link maker the harness stores are given.
func harnessUnsubscribeOption() jobs.Option {
	return jobs.WithUnsubscribe(harnessUnsubSigner.LinkMaker(harnessPublicBase))
}

var linkShape = regexp.MustCompile(`^https://platform\.example\.test/u/[A-Za-z0-9_-]{54}$`)

// jeDraftPreviewed creates and previews a job of n distinct addresses and returns it with its preview, funding or entitling the user for the module first.
func jeDraftPreviewed(e *jeEnv, user, module string, n int) (jobs.Job, jobs.Preview) {
	e.t.Helper()
	req := jobs.CreateReq{Module: module, Rows: jeAddrs(n, "r")}
	switch module {
	case jobs.ModuleSendBYO:
		e.enableM3(user)
		req.Content = jeContent()
	case jobs.ModuleSendManaged:
		e.fund(user, module, 1000)
		req.Content = jeContent()
	default:
		e.fund(user, module, 1000)
	}
	ctx := testCtx(e.t)
	j, err := e.jobs.CreateDraft(ctx, user, req)
	if err != nil {
		e.t.Fatalf("CreateDraft(%s): %v", module, err)
	}
	p, err := e.jobs.Preview(ctx, user, j.ID)
	if err != nil {
		e.t.Fatalf("Preview(%s): %v", j.ID, err)
	}
	return j, p
}

func TestJobsUnsubscribe_EnsureJobItemsWritesTheLinkOfEveryRowOfASendJob(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	for _, module := range []string{jobs.ModuleSendManaged, jobs.ModuleSendBYO} {
		j := e.confirmN(e.newUser(), module, 5)
		ids := e.rowIDs(j.ID, "pending")
		if n, err := e.jobs.EnsureJobItems(ctx, j.ID); err != nil || n != 5 {
			t.Fatalf("%s: EnsureJobItems = %d, %v", module, n, err)
		}
		seen := map[string]bool{}
		for _, id := range ids {
			link := e.str("SELECT COALESCE(unsubscribe_url, '<NULL>') FROM public.job_items WHERE id = $1", id)
			if !linkShape.MatchString(link) {
				t.Fatalf("%s: the link of item %s is %q, want https://<base>/u/<54 characters>", module, id, link)
			}
			// it is the very link the signer makes for that item, and its token verifies back to the item
			want, err := harnessUnsubSigner.LinkMaker(harnessPublicBase)(id)
			if err != nil || link != want {
				t.Fatalf("%s: the link of item %s is not the signer's (%v)", module, id, err)
			}
			if got, ok := harnessUnsubSigner.Verify(strings.TrimPrefix(link, harnessPublicBase+"/u/")); !ok || got != id {
				t.Fatalf("%s: the token in the link of %s names %q (ok=%v)", module, id, got, ok)
			}
			if seen[link] {
				t.Fatalf("%s: two items share the link %q", module, link)
			}
			seen[link] = true
		}
		if n := e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1 AND unsubscribe_url IS NULL", j.ID); n != 0 {
			t.Fatalf("%s: %d items of the job have no link", module, n)
		}
		// a replay writes nothing and changes nothing (the link is derived from the item id)
		before := e.rows("SELECT unsubscribe_url FROM public.job_items WHERE campaign_id = $1 ORDER BY id", j.ID)
		if n, err := e.jobs.EnsureJobItems(ctx, j.ID); err != nil || n != 0 {
			t.Fatalf("%s: a second EnsureJobItems = %d, %v", module, n, err)
		}
		if after := e.rows("SELECT unsubscribe_url FROM public.job_items WHERE campaign_id = $1 ORDER BY id", j.ID); strings.Join(after, "|") != strings.Join(before, "|") {
			t.Fatalf("%s: a replay changed the links", module)
		}
	}
}

func TestJobsUnsubscribe_AStoreWithNoMakerWritesNULLAndTheRendererSeesNothing(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	var seen []string
	rec := jeRenderer(func(c jobs.Content, row jobs.EngineRow) (string, string, string, error) {
		seen = append(seen, row.UnsubscribeURL)
		return c.Subject, "<p>x</p>", "", nil
	})
	// the job is confirmed by the store that has a maker (the confirm needs one); the items are written by stores of the two kinds, over the same database
	withMaker, err := jobs.New(e.pool, e.store, harnessUnsubscribeOption(), jobs.WithRenderer(rec))
	if err != nil {
		t.Fatal(err)
	}
	noMaker, err := jobs.New(e.pool, e.store, jobs.WithRenderer(rec))
	if err != nil {
		t.Fatal(err)
	}
	if noMaker.UnsubscribeConfigured() || !withMaker.UnsubscribeConfigured() {
		t.Fatal("the two stores are not the two kinds")
	}
	a := e.confirmN(e.newUser(), jobs.ModuleSendManaged, 2)
	b := e.confirmN(e.newUser(), jobs.ModuleSendManaged, 2)
	aIDs, bIDs := e.rowIDs(a.ID, "pending"), e.rowIDs(b.ID, "pending")

	// a store with no maker writes NO item: a send without a working unsubscribe link is not allowed, so a job confirmed before the dials went away waits instead of sending link-less messages
	n, err := noMaker.EnsureModuleItems(ctx, a.ID, aIDs)
	jeNotAllowed(t, "no maker: EnsureModuleItems", err, jobs.ReasonUnsubscribeNotConfigured)
	if n != 0 {
		t.Fatalf("no maker: EnsureModuleItems wrote %d items", n)
	}
	if _, err := noMaker.EnsureJobItems(ctx, a.ID); err == nil || !errors.Is(err, jobs.ErrNotAllowed) {
		t.Fatalf("no maker: EnsureJobItems = %v, want ErrNotAllowed", err)
	}
	if n := e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1", a.ID); n != 0 {
		t.Fatalf("no maker: %d items were written for job a, want none", n)
	}
	if len(seen) != 0 {
		t.Fatalf("no maker: the renderer was called %d times before the refusal", len(seen))
	}
	seen = nil

	// the control: the same call through the store that has a maker writes the link, and the renderer saw exactly the link that was written
	if n, err := withMaker.EnsureModuleItems(ctx, b.ID, bIDs); err != nil || n != 2 {
		t.Fatalf("with a maker: EnsureModuleItems = %d, %v", n, err)
	}
	if len(seen) != 2 {
		t.Fatalf("the renderer was called %d times for 2 rows", len(seen))
	}
	for i, id := range bIDs {
		col := e.str("SELECT COALESCE(unsubscribe_url, '<NULL>') FROM public.job_items WHERE id = $1", id)
		if !linkShape.MatchString(col) || seen[i] != col {
			t.Fatalf("row %d: the column holds %q and the renderer saw %q: they must be one string", i+1, col, seen[i])
		}
	}
}

func TestJobsUnsubscribe_AMakerThatReturnsAnUnusableLinkRefusesTheCallAndWritesNothing(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	const poison = "POISON-LINK-4471"
	for name, mk := range map[string]func(string) (string, error){
		"a link with a space":    func(id string) (string, error) { return "https://platform.example.test/u/a b-" + poison, nil },
		"an http link":           func(id string) (string, error) { return "http://platform.example.test/u/" + poison, nil },
		"an angle bracket":       func(id string) (string, error) { return "https://platform.example.test/u/<" + poison, nil },
		"a link over 2000 bytes": func(id string) (string, error) { return "https://" + strings.Repeat("a", 2000), nil },
		"an error":               func(id string) (string, error) { return "", errors.New("maker failed: " + poison) },
	} {
		bad, err := jobs.New(e.pool, e.store, jobs.WithUnsubscribe(mk))
		if err != nil {
			t.Fatal(err)
		}
		j := e.confirmN(e.newUser(), jobs.ModuleSendManaged, 3)
		_, err = bad.EnsureJobItems(ctx, j.ID)
		if err == nil || strings.Contains(err.Error(), poison) || strings.Contains(err.Error(), "platform.example.test") {
			t.Errorf("%s: EnsureJobItems = %v: it must refuse, and never quote the link", name, err)
		}
		if n := e.int("SELECT count(*) FROM public.job_items WHERE campaign_id = $1", j.ID); n != 0 {
			t.Errorf("%s: %d items were written by a refused call", name, n)
		}
		if e.str("SELECT COALESCE(items_ensured_at::text, 'no')  FROM platform.jobs WHERE id = $1", j.ID) != "no" {
			t.Errorf("%s: a refused call recorded that the items were written", name)
		}
	}
	// the control: the store with a good maker writes the same job's items
	j := e.confirmN(e.newUser(), jobs.ModuleSendManaged, 3)
	if n, err := e.jobs.EnsureJobItems(ctx, j.ID); err != nil || n != 3 {
		t.Fatalf("control failed: EnsureJobItems = %d, %v", n, err)
	}
}

func TestJobsUnsubscribe_ItemOwnerAnswersTheOwnerAndTheNormalisedAddressAndNothingElse(t *testing.T) {
	e := newJeEnv(t)
	ctx := testCtx(t)
	u1, u2 := e.newUser(), e.newUser()
	j1 := e.confirmN(u1, jobs.ModuleSendManaged, 3)
	j2 := e.confirmN(u2, jobs.ModuleSendBYO, 2)
	for _, c := range []struct{ job, user string }{{j1.ID, u1}, {j2.ID, u2}} {
		ids := e.rowIDs(c.job, "pending")
		if len(ids) < 2 {
			t.Fatalf("job %s has %d pending rows", c.job, len(ids))
		}
		for _, id := range ids {
			user, email, err := e.jobs.ItemOwner(ctx, id)
			if err != nil || user != c.user {
				t.Fatalf("ItemOwner(%s) = %q, %v, want the job's owner %s", id, user, err, c.user)
			}
			if want := e.str("SELECT email_norm FROM platform.job_rows WHERE id = $1", id); email != want || !strings.HasSuffix(email, "@example.test") {
				t.Fatalf("ItemOwner(%s) gave the address %q, the row's normalised address is %q", id, email, want)
			}
		}
	}
	// what is not a send job's pending row is the same ErrNotFound: a row of a verify job, a malformed row, a random id, a malformed id
	vj := e.confirmN(u1, jobs.ModuleVerify, 2)
	bad, err := e.jobs.CreateDraft(ctx, u2, jobs.CreateReq{Module: jobs.ModuleSendManaged, Content: jeContent(), Rows: []jobs.RowIn{{Email: "not-an-address"}, {Email: "good.address@example.test"}}})
	if err != nil {
		t.Fatal(err)
	}
	malformed := e.rows("SELECT id::text FROM platform.job_rows WHERE job_id = $1 AND state = 'malformed'", bad.ID)
	if len(malformed) != 1 {
		t.Fatalf("the draft has %d malformed rows, want 1", len(malformed))
	}
	for name, id := range map[string]string{
		"a row of a verify job": e.rowIDs(vj.ID, "pending")[0],
		"a malformed row":       malformed[0],
		"a random uuid":         "9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d",
		"the nil uuid":          "00000000-0000-0000-0000-000000000000",
		"an upper-case id":      strings.ToUpper(e.rowIDs(j1.ID, "pending")[0]),
		"not an id":             "' OR true --",
		"empty":                 "",
	} {
		user, email, err := e.jobs.ItemOwner(ctx, id)
		if !errors.Is(err, jobs.ErrNotFound) || user != "" || email != "" {
			t.Errorf("%s: ItemOwner = (%q, %q, %v), want ErrNotFound and nothing", name, user, email, err)
		}
		jeNoRawSQL(t, name, err)
	}
}

func TestJobsUnsubscribe_AStoreWithNoMakerRefusesToConfirmASendJobAndHoldsNothing(t *testing.T) {
	e := newJeEnv(t, jobs.WithUnsubscribe(nil))
	ctx := testCtx(t)
	if e.jobs.UnsubscribeConfigured() {
		t.Fatal("the store has a maker: WithUnsubscribe(nil) did not clear the harness default")
	}
	for _, module := range []string{jobs.ModuleSendManaged, jobs.ModuleSendBYO} {
		for _, scheduled := range []bool{false, true} {
			u := e.newUser()
			j, p := jeDraftPreviewed(e, u, module, 3)
			availBefore, resBefore, _ := e.bal(u, module)
			req := jobs.ConfirmReq{JobID: j.ID, Fingerprint: p.Fingerprint, Key: e.key()}
			if scheduled {
				start := time.Now().Add(2 * time.Hour)
				req.ScheduledStart = &start
			}
			_, err := e.jobs.Confirm(ctx, u, req)
			jeNotAllowed(t, module+": a confirm with no link maker", err, jobs.ReasonUnsubscribeNotConfigured)
			if got := e.jobStatus(j.ID); got != jobs.StatusPreviewed {
				t.Fatalf("%s: the refused job is %s, want it still previewed", module, got)
			}
			if n := e.int("SELECT count(*) FROM platform.reserves WHERE job_id = $1", j.ID); n != 0 {
				t.Fatalf("%s: a refused confirm reserved credits", module)
			}
			if a, r, _ := e.bal(u, module); a != availBefore || r != resBefore {
				t.Fatalf("%s: the balance moved by a refused confirm: %d/%d, was %d/%d", module, a, r, availBefore, resBefore)
			}
			if n := e.int("SELECT count(*) FROM platform.job_results WHERE job_id = $1", j.ID); n != 0 {
				t.Fatalf("%s: a refused confirm wrote %d results", module, n)
			}
		}
	}

	// the same store confirms a VERIFY job (it has no message to carry a link)
	u := e.newUser()
	vj, vp := jeDraftPreviewed(e, u, jobs.ModuleVerify, 3)
	if res, err := e.jobs.Confirm(ctx, u, jobs.ConfirmReq{JobID: vj.ID, Fingerprint: vp.Fingerprint, Key: e.key()}); err != nil || res.Job.Status != jobs.StatusConfirmed {
		t.Fatalf("a verify job in a store with no link maker: %v, %v", res.Job.Status, err)
	}
	// a job that does not exist for the user is ErrNotFound, as everywhere (the refusal does not tell whose job it is)
	sj, sp := jeDraftPreviewed(e, e.newUser(), jobs.ModuleSendManaged, 1)
	if _, err := e.jobs.Confirm(ctx, u, jobs.ConfirmReq{JobID: sj.ID, Fingerprint: sp.Fingerprint, Key: e.key()}); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("another user's send job: %v, want ErrNotFound", err)
	}

	// the control, and the paired positive: with a maker the very same job confirms, and a replay of that confirm is answered as a replay by a store that has since lost its maker
	withMaker, err := jobs.New(e.pool, e.store, harnessUnsubscribeOption())
	if err != nil {
		t.Fatal(err)
	}
	u2 := e.newUser()
	j2, p2 := jeDraftPreviewed(e, u2, jobs.ModuleSendManaged, 3)
	key := e.key()
	res, err := withMaker.Confirm(ctx, u2, jobs.ConfirmReq{JobID: j2.ID, Fingerprint: p2.Fingerprint, Key: key})
	if err != nil || res.Job.Status != jobs.StatusConfirmed || res.Replayed {
		t.Fatalf("control failed: a send job in a store with a maker: %v, %v", res.Job.Status, err)
	}
	again, err := e.jobs.Confirm(ctx, u2, jobs.ConfirmReq{JobID: j2.ID, Fingerprint: p2.Fingerprint, Key: key})
	if err != nil || !again.Replayed || again.Job.Status != jobs.StatusConfirmed {
		t.Fatalf("a replay of a confirmed job in a store with no maker: replayed=%v status=%s err=%v, want the replay", again.Replayed, again.Job.Status, err)
	}
}

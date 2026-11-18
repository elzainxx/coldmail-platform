package m3_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// goodRun is a run nothing stands in the way of: entitled, nothing active, 100 recipients, caps far above.
func goodRun() m3.RunFacts {
	return m3.RunFacts{Entitled: true, RecipientCount: 100, QueueDepthCap: m3.KnownLimit(1000), CampaignSizeCap: m3.KnownLimit(500)}
}

var causes = []error{m3.ErrNotEntitled, m3.ErrJobActive, m3.ErrOverCampaignCap, m3.ErrOverQueueCap}

// which lists the causes an error names (by errors.Is), in the order of `causes`.
func which(err error) []error {
	var out []error
	for _, c := range causes {
		if errors.Is(err, c) {
			out = append(out, c)
		}
	}
	return out
}

// preCheckProblems runs the whole table of the pre-check against fn and returns what is wrong, so that the same assertion can be run on a deliberately broken variant and be shown to catch
// it: for each cause, the facts that break exactly that one are refused with exactly that cause and with the Invalid kind, the same facts one step on the right side of the line pass, and a
// run that breaks nothing passes.
func preCheckProblems(fn func(m3.RunFacts) error) []string {
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	if err := fn(goodRun()); err != nil {
		add("a run nothing stands in the way of is refused: %v", err)
	}
	type tc struct {
		name  string
		bad   func(*m3.RunFacts)
		good  func(*m3.RunFacts)
		cause error
	}
	for _, c := range []tc{
		{"not entitled", func(f *m3.RunFacts) { f.Entitled = false }, func(f *m3.RunFacts) { f.Entitled = true }, m3.ErrNotEntitled},
		{"a job is active", func(f *m3.RunFacts) { f.JobActive = true }, func(f *m3.RunFacts) { f.JobActive = false }, m3.ErrJobActive},
		{"one over the campaign cap", func(f *m3.RunFacts) { f.RecipientCount = 501 }, func(f *m3.RunFacts) { f.RecipientCount = 500 }, m3.ErrOverCampaignCap},
		{"one over the queue cap", func(f *m3.RunFacts) { f.RecipientCount = 1001; f.CampaignSizeCap = m3.Limit{} }, func(f *m3.RunFacts) { f.RecipientCount = 1000; f.CampaignSizeCap = m3.Limit{} }, m3.ErrOverQueueCap},
		{"live jobs push it over the queue cap", func(f *m3.RunFacts) { f.RecipientCount = 100; f.LiveQueuedJobs = 901 }, func(f *m3.RunFacts) { f.RecipientCount = 100; f.LiveQueuedJobs = 900 }, m3.ErrOverQueueCap},
	} {
		bad := goodRun()
		c.bad(&bad)
		err := fn(bad)
		got := which(err)
		if len(got) != 1 || got[0] != c.cause {
			add("%s: the causes named are %v, want only %v (err %v)", c.name, got, c.cause, err)
		} else if modclient.KindOf(err) != modclient.Invalid {
			add("%s: the kind is %s, want invalid (refused before anything was sent)", c.name, modclient.KindOf(err))
		}
		ok := bad
		c.good(&ok)
		if err := fn(ok); err != nil {
			add("%s: the boundary one step on the right side is refused: %v", c.name, err)
		}
	}
	return problems
}

// PreCheckRun refuses from the platform's own state, one typed cause per fact, before any network call; each cause has its boundary (the cap itself passes, one past it is refused) and its
// paired control: the same facts with that one fact set right pass. The assertion is shown to catch a broken variant: a pre-check that ignores a cause, one that is off by one at the cap,
// one that forgets the live queue, and one that names the wrong cause are each reported.
func TestPreCheckRunNamesEachCauseAndOnlyThat(t *testing.T) {
	if p := preCheckProblems(m3.PreCheckRun); len(p) != 0 {
		t.Fatalf("the pre-check is wrong: %v", p)
	}
	// the controls: the same assertion on broken variants
	broken := map[string]struct {
		fn   func(m3.RunFacts) error
		want string
	}{
		"ignores entitlement": {func(f m3.RunFacts) error { f.Entitled = true; return m3.PreCheckRun(f) }, "not entitled"},
		"ignores a job":       {func(f m3.RunFacts) error { f.JobActive = false; return m3.PreCheckRun(f) }, "a job is active"},
		"campaign cap off by one": {func(f m3.RunFacts) error {
			if f.CampaignSizeCap.Known {
				f.CampaignSizeCap.N++
			}
			return m3.PreCheckRun(f)
		}, "one over the campaign cap"},
		"queue cap off by one": {func(f m3.RunFacts) error {
			if f.QueueDepthCap.Known {
				f.QueueDepthCap.N--
			}
			return m3.PreCheckRun(f)
		}, "one over the queue cap"},
		"forgets the live queue": {func(f m3.RunFacts) error { f.LiveQueuedJobs = 0; return m3.PreCheckRun(f) }, "live jobs push it over"},
		"names the wrong cause": {func(f m3.RunFacts) error {
			if err := m3.PreCheckRun(f); errors.Is(err, m3.ErrOverQueueCap) {
				return m3.ErrOverCampaignCap
			} else {
				return err
			}
		}, "one over the queue cap"},
	}
	for name, b := range broken {
		if p := preCheckProblems(b.fn); len(p) == 0 || !anyHas(p, b.want) {
			t.Fatalf("a pre-check that %s was not caught for %q: %v", name, b.want, p)
		}
	}
}

// When several causes apply, all of them are named, in the order the module itself tests them, and none that does not apply; the kind is still Invalid. A fact that cannot describe a run is an
// Invalid error and names no cause (nothing is decided from nonsense), and facts nobody filled in never pass.
func TestPreCheckRunNamesEveryCauseThatApplies(t *testing.T) {
	f := goodRun()
	f.Entitled, f.JobActive, f.RecipientCount = false, true, 5000
	f.CampaignSizeCap, f.QueueDepthCap = m3.KnownLimit(1000), m3.KnownLimit(2000)
	err := m3.PreCheckRun(f)
	if got := which(err); len(got) != 4 || got[0] != m3.ErrNotEntitled || got[1] != m3.ErrJobActive || got[2] != m3.ErrOverCampaignCap || got[3] != m3.ErrOverQueueCap {
		t.Fatalf("causes: %v (%v)", got, err)
	}
	if modclient.KindOf(err) != modclient.Invalid {
		t.Fatalf("kind %s", modclient.KindOf(err))
	}
	// only the causes that apply
	f.Entitled, f.JobActive = true, false
	if got := which(m3.PreCheckRun(f)); len(got) != 2 || got[0] != m3.ErrOverCampaignCap || got[1] != m3.ErrOverQueueCap {
		t.Fatalf("causes: %v", got)
	}
	// nonsense is not a cause
	for name, bad := range map[string]m3.RunFacts{
		"no recipients":  {Entitled: true},
		"negative count": {Entitled: true, RecipientCount: -3},
		"negative live":  {Entitled: true, RecipientCount: 5, LiveQueuedJobs: -1},
		"negative cap":   {Entitled: true, RecipientCount: 5, QueueDepthCap: m3.KnownLimit(-1)},
		"negative cap 2": {Entitled: true, RecipientCount: 5, CampaignSizeCap: m3.KnownLimit(-1)},
		"the zero facts": {},
	} {
		err := m3.PreCheckRun(bad)
		if err == nil || modclient.KindOf(err) != modclient.Invalid || len(which(err)) != 0 {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// a cap of zero is a cap (nothing fits), not an unknown cap
	if err := m3.PreCheckRun(m3.RunFacts{Entitled: true, RecipientCount: 1, QueueDepthCap: m3.KnownLimit(0)}); !errors.Is(err, m3.ErrOverQueueCap) {
		t.Fatalf("a known cap of 0: %v", err)
	}
	// no sum of two huge counts wraps into a small one
	huge := m3.RunFacts{Entitled: true, RecipientCount: 1 << 62, LiveQueuedJobs: 1 << 62, QueueDepthCap: m3.KnownLimit(100)}
	if err := m3.PreCheckRun(huge); !errors.Is(err, m3.ErrOverQueueCap) {
		t.Fatalf("a count that would overflow a sum: %v", err)
	}
}

// A check that cannot be made says so: an unknown cap is not checked (and not checked against zero), and NotChecked lists exactly the checks the facts leave out. The control: the same facts
// with the caps known list nothing and refuse what is over them.
func TestPreCheckRunSaysWhatItDidNotCheck(t *testing.T) {
	f := m3.RunFacts{Entitled: true, RecipientCount: 1 << 40}
	if err := m3.PreCheckRun(f); err != nil {
		t.Fatalf("with no cap known, a huge campaign is not refused by a cap nobody knows: %v", err)
	}
	if got := f.NotChecked(); len(got) != 2 || got[0] != "campaign_size_cap" || got[1] != "queue_depth_cap" {
		t.Fatalf("not checked: %v", got)
	}
	if got := (m3.RunFacts{QueueDepthCap: m3.KnownLimit(5)}).NotChecked(); len(got) != 1 || got[0] != "campaign_size_cap" {
		t.Fatalf("not checked: %v", got)
	}
	f.QueueDepthCap, f.CampaignSizeCap = m3.KnownLimit(1000), m3.KnownLimit(2000)
	if got := f.NotChecked(); len(got) != 0 {
		t.Fatalf("not checked: %v", got)
	}
	if err := m3.PreCheckRun(f); !errors.Is(err, m3.ErrOverQueueCap) || !errors.Is(err, m3.ErrOverCampaignCap) {
		t.Fatalf("with the caps known the same campaign is refused: %v", err)
	}
}

// What the module states, and what only the platform can supply: the queue depth cap comes from the quote (the module states it), the campaign size cap does not (the module computes it and
// never says it).
func TestTheQuoteStatesTheQueueCapAndNotTheCampaignCap(t *testing.T) {
	s := newStack(t, replyFixture(200, "quote.feasible.json", t), modclient.Options{})
	n := quoteCount(t, "quote.feasible.json")
	f, err := s.client(t, userA).Feasibility(ctxT(t), n)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.QueueDepthLimit(); !got.Known || got.N != asInt(t, doc(t, "quote.feasible.json"), "caps.queue_depth_cap") {
		t.Fatalf("the queue depth limit: %+v", got)
	}
	facts := m3.RunFacts{Entitled: true, RecipientCount: n, QueueDepthCap: f.QueueDepthLimit()}
	if err := m3.PreCheckRun(facts); err != nil {
		t.Fatal(err)
	}
	if got := facts.NotChecked(); len(got) != 1 || got[0] != "campaign_size_cap" {
		t.Fatalf("not checked: %v", got)
	}
	// the shape of Caps has no campaign size field: nothing in the quote could fill one
	if _, ok := reflectField(m3.Caps{}, "CampaignSizeCap"); ok {
		t.Fatal("Caps names a campaign size cap the module does not state")
	}
}

// ---- the unexplained 409 -----------------------------------------------------------------------------------------------------

// After the module answers a run with 409 anyway, ExplainConflict records "refused, cause unknown" and NEVER a cause: the result is ErrConflictUnexplained and the Refused error underneath, and
// it is none of the four causes, whatever the platform's own facts say would explain it, and whatever the body claims (a detail naming a field, a different sentence). The control: an error
// that is not run's 409 passes through untouched.
func TestExplainConflictNeverPicksACause(t *testing.T) {
	bodies := map[string]string{
		"the recorded body":       string(sendapitest.Read(t, fixtures, "run.error.conflict.json")),
		"a body that names one":   sendapitest.Envelope("conflict", [2]string{"max_campaign_size", "conflict"}),
		"a body that says why":    `{"error":{"code":"conflict","message":"The campaign is full.","details":[{"field":"campaign_id","issue":"conflict"},{"field":"user_id","issue":"conflict"}],"request_id":"req_0123456789abcdef0123456789abcdef"}}`,
		"the suspension sentence": `{"error":{"code":"conflict","message":"This action is not available for your account right now.","request_id":"req_0123456789abcdef0123456789abcdef"}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			s := newStack(t, reply(409, body), modclient.Options{})
			_, err := s.client(t, userA).Run(ctxT(t), jobItem)
			wantKind(t, err, modclient.Refused)
			got := m3.ExplainConflict(err)
			if !errors.Is(got, m3.ErrConflictUnexplained) || !errors.Is(got, sendapi.ErrConflictUnexplained) || !errors.Is(got, modclient.ErrRefused) {
				t.Fatalf("the explanation is not 'cause unknown': %v", got)
			}
			if c := which(got); len(c) != 0 {
				t.Fatalf("a cause was picked: %v", c)
			}
			if errors.Is(got, sendapi.ErrBackpressure) || errors.Is(got, m3.ErrAccountDuplicate) || errors.Is(got, m3.ErrActionUnavailable) {
				t.Fatalf("another module's or route's meaning was picked: %v", got)
			}
			var e *modclient.Error
			if !errors.As(got, &e) || e.Status != 409 || e.Route != "m3.run" || !strings.Contains(got.Error(), "cause unknown") {
				t.Fatalf("the Refused error is not underneath: %v", got)
			}
			// idempotent: explaining an explanation changes nothing
			if again := m3.ExplainConflict(got); !errors.Is(again, m3.ErrConflictUnexplained) || len(which(again)) != 0 {
				t.Fatalf("explained twice: %v", again)
			}
		})
	}
	// the facts of the platform that would explain it do not leak in: ExplainConflict takes the error and nothing else, and the same 409 explained twice is the same answer
	facts := goodRun()
	facts.JobActive = true
	if err := m3.PreCheckRun(facts); !errors.Is(err, m3.ErrJobActive) {
		t.Fatal("the control: the facts do name a cause")
	}
	// controls: what is not run's 409 passes through untouched
	for name, tc := range map[string]struct {
		h    handler
		call func(c *m3.Client) error
	}{
		"a 404 on run":        {reply(404, sendapitest.Envelope("not_found", [2]string{"job_item_id", "not_found"})), func(c *m3.Client) error { _, err := c.Run(ctxT(t), jobItem); return err }},
		"a 503 paused on run": {reply(503, sendapitest.Envelope("paused")), func(c *m3.Client) error { _, err := c.Run(ctxT(t), jobItem); return err }},
		"a 409 on create":     {reply(409, sendapitest.Envelope("conflict")), func(c *m3.Client) error { _, err := c.CreateAccount(ctxT(t), newAcct(t, 1)); return err }},
		"a 409 on the test":   {reply(409, sendapitest.Envelope("conflict")), func(c *m3.Client) error { _, err := c.TestAccount(ctxT(t), acct1, false); return err }},
	} {
		s := newStack(t, tc.h, modclient.Options{})
		err := tc.call(s.client(t, userA))
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		if got := m3.ExplainConflict(err); got != err {
			t.Fatalf("%s: an error that is not run's 409 was changed: %v", name, got)
		}
	}
	if m3.ExplainConflict(nil) != nil {
		t.Fatal("nil is not an error")
	}
	plain := errors.New("not a module error")
	if m3.ExplainConflict(plain) != plain {
		t.Fatal("a foreign error was changed")
	}
	// a Refused 409 built by hand for another route is not run's
	hand := &modclient.Error{Kind: modclient.Refused, Module: modclient.M3, Route: "m3.quote", Status: http.StatusConflict}
	if m3.ExplainConflict(hand) != error(hand) {
		t.Fatal("a 409 of another route was explained as run's")
	}
}

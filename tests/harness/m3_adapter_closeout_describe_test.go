//go:build harness

package harness

import (
	"context"
	"slices"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// TestM3Adapter_DescribeAdvertisesWhatTheAdapterHandles (Phase 3 close-out C1, C2, C5, C6 / D5): the adapter's closed sets are compared with what the REAL module 3 publishes in its describe, the check
// that tells a person BEFORE any money or message moves that the module added or lost a value. The module 3 of the close-out publishes `cancelled` LAST in job_state, the new vocabulary
// run_conflict_cause (the four causes of a refused run), the three new quote reason codes and capabilities.plain_text_alternative. The check is CheckVocabularies (job_state, error_class,
// run_conflict_cause and the nine error codes: a break is a Contract error naming the vocabulary) and CheckAccountVocabularies (the sets of the account side). The paired control is the same describe
// compared with a profile that does not know the state `cancelled`: the comparison must call that a break (a new state is never mapped by guesswork).
func TestM3Adapter_DescribeAdvertisesWhatTheAdapterHandles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), m3aBudget)
	t.Cleanup(cancel)
	e := newM3aEnv(t, ctx)

	d, err := e.ad.Describe(ctx)
	if err != nil {
		t.Fatalf("Describe through the adapter failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	rep, err := e.ad.CheckVocabularies(ctx)
	if err != nil {
		t.Fatalf("CheckVocabularies against the real module 3 failed (%s): %s (the module's describe no longer matches what the adapter handles)", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	names := map[string]bool{}
	for _, df := range rep.Diffs {
		names[df.Name] = true
		if !df.Advertised || !df.Exact() {
			t.Errorf("the vocabulary %s is advertised %v with missing %v and extra %v, want exactly what the adapter handles", df.Name, df.Advertised, df.Missing, df.Extra)
		}
	}
	for _, want := range []string{"job_state", "error_class", "run_conflict_cause"} {
		if !names[want] {
			t.Errorf("CheckVocabularies compared no %s: the module 3 profile must handle it", want)
		}
	}
	if rep.Breaking() || len(rep.CodesMissing) != 0 {
		t.Errorf("the comparison is breaking (%+v)", rep)
	}
	vs, err := e.ad.Vocabularies(ctx)
	if err != nil {
		t.Fatalf("Vocabularies: %s", e.raw.scrub(err.Error()))
	}
	if js := vs["job_state"]; len(js) == 0 || js[len(js)-1] != "cancelled" || !slices.Equal(js, m3.Profile().JobStates) {
		t.Errorf("the module's job_state is %v, want the adapter's %v with cancelled last", js, m3.Profile().JobStates)
	}
	if !slices.Equal(vs["run_conflict_cause"], []string{"suspended", "campaign_active", "campaign_cap", "queue_cap"}) {
		t.Errorf("the module's run_conflict_cause is %v, want the four causes in the module's order", vs["run_conflict_cause"])
	}
	if !d.Capabilities["plain_text_alternative"] {
		t.Errorf("the module's capabilities say plain_text_alternative %v, want true (the plain-text part of the close-out)", d.Capabilities["plain_text_alternative"])
	}
	diffs, err := e.ad.CheckAccountVocabularies(ctx)
	if err != nil {
		t.Fatalf("CheckAccountVocabularies against the real module 3 failed (%s): %s", modclient.KindOf(err), e.raw.scrub(err.Error()))
	}
	for _, df := range diffs {
		if df.Breaking() {
			t.Errorf("the account vocabulary %s is breaking: missing %v", df.Name, df.Missing)
		}
		if len(df.Extra) != 0 {
			t.Logf("NOTE m3a describe: the module publishes %v more than the adapter handles in %s (additive: the adapter carries them as the Unrecognised word)", df.Extra, df.Name)
		}
	}
	// the control: the same describe against a profile that has no cancelled state is a break
	old := m3.Profile()
	old.JobStates = slices.DeleteFunc(slices.Clone(old.JobStates), func(s string) bool { return s == "cancelled" })
	old.Cancel = false
	if r := sendapi.CompareVocabularies(d, old); !r.Breaking() || len(r.Diffs) == 0 || r.Diffs[0].Name != "job_state" || !slices.Equal(r.Diffs[0].Extra, []string{"cancelled"}) {
		t.Errorf("the control: the module's describe against a profile with no cancelled state is %+v, want a break naming job_state with the extra value cancelled", r)
	}
	t.Logf("NOTE m3a describe: job_state %v, run_conflict_cause %v, plain_text_alternative %v; %d vocabularies compared exactly, %d account-side sets compared", vs["job_state"], vs["run_conflict_cause"], d.Capabilities["plain_text_alternative"], len(rep.Diffs), len(diffs))
}

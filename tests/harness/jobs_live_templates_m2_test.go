//go:build harness && m2worker

package harness

import (
	"net/http"
	"testing"

	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 5 S9, the live proofs of templates and unsubscribe against module 2 (METERED): the scenarios and their comparisons are in jobs_live_templates_helpers_test.go, module 3's rig and the two comparison controls in
// jobs_live_templates_test.go. This file carries the tag m2worker because module 2's real Control and worker are only part of the stack when that stage is (the full run includes it, run.sh FULL_INCLUDES).

// jltM2Rig is the rig of module 2: its REAL Control, its REAL worker (one, so one send at a time; one lane per user) and the controllable sink, a user with a pool of its own, an account naming the sink and 1000
// credits. The credits are what makes module 2 different from module 3 here: the reserve must hold the rows that can be sent and not the rows that cannot, and a row that is left out is never charged.
func jltM2Rig(t *testing.T) *jltRig {
	t.Helper()
	const grant = 1000
	m := newJlM2Env(t)
	r := m.r
	s, tr := m.stack(r.m2URL, 0)
	s.jltUseTemplates(tr)
	tenants := map[string]*m2wTenant{}
	return &jltRig{
		T: t, S: s, Module: jobs.ModuleSendManaged, Label: "module 2", Metered: true, Grant: grant, Run: "jlt" + randomHex(t, 4) + "-",
		NewUser: func(name string) string {
			tn := m.tenant(s, name)
			s.grant(tn.UserID, jobs.ModuleSendManaged, grant)
			tenants[tn.UserID] = tn
			return tn.UserID
		},
		Ctl:    func(method, path string) []byte { return r.ctlCall(t, method, path, nil, http.StatusOK) },
		Attach: func(user, job string) { tenants[user].CampaignID = job },
		Diag:   func(user string) string { return r.diagnostics(t, tenants[user]) },
	}
}

// TestJobsLive_Templates_M2_RenderedMessagesAtTheSink (Phase 5 S9, exit criteria 1 to 3, METERED, module 2's REAL Control and worker): the scenario of TestJobsLive_Templates_M3_RenderedMessagesAtTheSink against
// module 2, with its money: the reserve holds the fifteen rows that can be sent (not the seventeen of the list), exactly fifteen are charged, the two rows that lack the company have no ledger row and are not in the
// reserve, and the remainder is released.
func TestJobsLive_Templates_M2_RenderedMessagesAtTheSink(t *testing.T) {
	jltRunTemplates(jltM2Rig(t))
}

// TestJobsLive_Unsubscribe_M2_OneClickLandsOnSuppressionAndTheNextJobExcludesIt (Phase 5 S9, exit criterion 4, METERED, module 2's REAL Control and worker): the scenario of the module 3 test against module 2, with the
// platform's footer carrying the link (module 3's test places it in the template, so both ways of having the link are read at a sink), and the second job's reserve holds twelve rows, not fourteen.
func TestJobsLive_Unsubscribe_M2_OneClickLandsOnSuppressionAndTheNextJobExcludesIt(t *testing.T) {
	jltRunUnsubscribe(jltM2Rig(t), false)
}

//go:build harness

package harness

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"slices"
	"strings"
	"testing"
)

// Helpers of the Phase 3 close-out tests of module 3's adapter (chunk D5): m3_adapter_closeout_cancel_test.go, _conflict_test.go, _codes_test.go, _hostguard_test.go and _plaintext_test.go. Every
// name starts with `m3c`. They build on m3aEnv (m3_adapter_e2e_helpers_test.go): the tenant, the account, the adapter, the controllable sink, the dials and the module's own tables.
//
// THE CLOSED SETS are written out here, from the module's documentation (Docs/api-contract.md, "Closed vocabularies of the test button and bulk-add bodies" and "The four causes of a run 409"),
// and not read from the adapter: the live test must be able to say that an answer is outside the module's own sets even if the adapter's list and the module's drifted together.

var (
	m3cVerifyCodes      = []string{"verify_connect_failed", "verify_timeout", "verify_tls_unavailable", "verify_tls_failed", "verify_auth_failed", "verify_protocol_error", "verify_connection_lost", "verify_host_not_allowed", "verify_error", "verification_unavailable"}
	m3cVerifyStates     = []string{"verified", "failed", "unverified", "unavailable"}
	m3cAlignStates      = []string{"aligned", "unaligned_warned", "blocked", "unverified", "indeterminate"}
	m3cAlignCodes       = []string{"from_dns_unavailable", "from_domain_not_found"}
	m3cBulkRowStatuses  = []string{"created", "rejected"}
	m3cBulkRejectCodes  = []string{"validation_failed", "account_cap_reached", "duplicate_account_for_user", "from_alignment_warning_unacknowledged", "conflict", "unprocessable", "not_found", "internal_error"}
	m3cInternalMarkers  = []string{"unique_violation", "check_violation", "fk_violation", "tx_begin_error", "account_write_error", "encrypt_error", "from_gate_write_error", "from_gate_acknowledgment_write_error", "SQLSTATE", "duplicate key", "pgx", "dial tcp", "goroutine"}
	m3cRunCauseWords    = []string{"suspended", "campaign_active", "campaign_cap", "queue_cap"}
	m3cBulkFreeTextKeys = map[string]bool{"smtp_response": true, "from_alignment_message": true, "request_id": true}
)

// m3cKinds is n recipients of one kind.
func m3cKinds(kind string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = kind
	}
	return out
}

func m3cIn(set []string, v string) bool { return slices.Contains(set, v) }

// seedItemsIn writes job items for the tenant into the GIVEN campaign (seedTenant writes one campaign, seedExtraItem one item per new campaign): the platform's own INSERT shape, one row per label,
// each recipient carrying this run's prefix so that the sink's view of the tenant finds it.
func (e *m3aEnv) seedItemsIn(t *testing.T, tn *m3aTenant, campaign string, labels ...string) []*m3aItem {
	t.Helper()
	var out []*m3aItem
	for _, label := range labels {
		email := fmt.Sprintf("%s%s@example.test", tn.Prefix, label)
		var id string
		err := e.db.QueryRow(e.ctx, `INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5) RETURNING id::text`,
			campaign, tn.UserID, email, "[m3adapter "+e.runID+"] "+label, "<p>m3adapter "+e.runID+" "+label+"</p>").Scan(&id)
		if err != nil {
			t.Fatalf("seed the job item %s of %s in campaign %s: %v", label, tn.Name, campaign, err)
		}
		out = append(out, &m3aItem{Index: len(out) + 1, ID: id, Email: email, Kind: "ok"})
	}
	return out
}

// userAccounts is the number of live (not soft-deleted) accounts module 3 holds for the user: the ground truth beside the adapter's list.
func (e *m3aEnv) userAccounts(t *testing.T, userID string) int {
	t.Helper()
	return e.count(t, "count the accounts of the user", "SELECT count(*) FROM send_byo.accounts WHERE user_id = $1::uuid AND deleted_at IS NULL", userID)
}

// jobCount is the number of jobs module 3 holds for the user, optionally of one state ("" for every state).
func (e *m3aEnv) jobCount(t *testing.T, userID, state string) int {
	t.Helper()
	if state == "" {
		return e.count(t, "count the jobs of the user", "SELECT count(*) FROM send_byo.jobs WHERE user_id = $1::uuid", userID)
	}
	return e.count(t, "count the jobs of the user in a state", "SELECT count(*) FROM send_byo.jobs WHERE user_id = $1::uuid AND state::text = $2", userID, state)
}

// ---- the parsed message (the controllable sink's raw bytes, GET /api/message?seq=N) ----

type m3cMessage struct {
	ctype  string
	params map[string]string
	parts  []m3cPart // multipart/alternative: its parts in order; otherwise empty
	body   string    // a single-part message: its decoded body
}

type m3cPart struct{ ctype, charset, cte, body string }

func m3cDecode(cte string, raw []byte) (string, error) {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "quoted-printable":
		b, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
		return string(b), err
	case "base64":
		b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
		return string(b), err
	}
	return string(raw), nil // 7bit, 8bit, binary or none: the bytes are the text
}

func m3cParse(t *testing.T, raw []byte) m3cMessage {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the raw message does not parse as a mail message (%v):\n%.600s", err, raw)
	}
	ct := msg.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("the message has no readable Content-Type %q (%v)", ct, err)
	}
	out := m3cMessage{ctype: mt, params: params}
	if !strings.HasPrefix(mt, "multipart/") {
		body, _ := io.ReadAll(msg.Body)
		out.body, err = m3cDecode(msg.Header.Get("Content-Transfer-Encoding"), body)
		if err != nil {
			t.Fatalf("the single-part body does not decode under its Content-Transfer-Encoding: %v", err)
		}
		return out
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("a part of the multipart message does not parse: %v", err)
		}
		pt, pp, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		b, _ := io.ReadAll(p)
		dec, err := m3cDecode(p.Header.Get("Content-Transfer-Encoding"), b)
		if err != nil {
			t.Fatalf("a part does not decode under its Content-Transfer-Encoding: %v", err)
		}
		out.parts = append(out.parts, m3cPart{ctype: pt, charset: strings.ToLower(pp["charset"]), cte: p.Header.Get("Content-Transfer-Encoding"), body: dec})
	}
	return out
}

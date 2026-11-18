package m1

import (
	"context"
	"net/http"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/norm"
)

// Verdict is the answer to a single-address check: what module 1 said, reduced to what the platform shows and charges on, or (Malformed set) the fact that the address was never
// sent.
type Verdict struct {
	// Status, Reason, Confidence and Recommendation are module 1's verdict (empty and 0 when Malformed is set).
	Status         string
	Reason         string
	Confidence     int
	Recommendation string
	// Domain is the normalised domain module 1 judged ("" when it could not extract one).
	Domain string
	// FromCache, SkipReason and FailureOrigin are the charge table's other inputs (flags.from_cache, signals.smtp_skipped_reason, signals.failure_origin).
	FromCache     bool
	SkipReason    string
	FailureOrigin string
	// Malformed is the norm reason (norm/v1 or module 1's syntax layer) when the address failed the platform's address rule: module 1 was NOT called, and Outcome is the free platform / malformed one (row P-08).
	Malformed norm.Reason
}

// Outcome is what the charge table reads of the verdict.
func (v Verdict) Outcome() charge.Outcome {
	if v.Malformed != "" {
		return MalformedOutcome()
	}
	return charge.Outcome{Module: "m1", State: v.Status, Reason: v.Reason, FromCache: v.FromCache, SkipReason: v.SkipReason, FailureOrigin: v.FailureOrigin}
}

type verifyBody struct {
	Email string `json:"email"`
}

// Verify checks one address synchronously: POST /v1/verify. An address that fails the platform's address rule (norm/v1 and module 1's syntax layer) is never sent (the verdict says Malformed and no request is made); every other address is
// sent as given and the verdict is returned with module 1's own words, each checked against its closed vocabulary.
//
// IT IS NEVER RETRIED, by the adapter or by modclient (the request is not marked idempotent). Module 1's cache is OFF unless TM_CACHE_TTL_SECONDS is set (research m1 1.8 item
// 14), so a second send of the same address after a timeout or a reset repeats the work: another DNS walk, another SMTP session to the recipient's server (a second contact, which
// can trip the provider's rate limit on our node) and, with the vendor fallback on, another billed vendor call; and the first attempt may have completed unseen. The caller decides
// whether to ask again; a Timeout or Unavailable error says only that nothing was confirmed. The per-request timeout is at least verifyTimeoutFloor because the module holds the
// request for the node's whole session.
func (a *Adapter) Verify(ctx context.Context, address string) (Verdict, error) {
	if _, err := norm.Validate(address); err != nil {
		reason, _ := norm.ReasonAny(err)
		return Verdict{Malformed: reason}, nil
	}
	resp, err := a.c.Do(ctx, modclient.Request{
		Module: modclient.M1, Route: routeVerify, Method: http.MethodPost, Path: "/v1/verify",
		Body: verifyBody{Email: address}, Timeout: max(a.timeout, verifyTimeoutFloor), Idempotent: false,
	})
	if err != nil {
		return Verdict{}, err
	}
	var w struct {
		Status         string `json:"status"`
		Reason         string `json:"reason"`
		Confidence     int    `json:"confidence"`
		Recommendation string `json:"recommendation"`
		Domain         string `json:"domain"`
		Flags          struct {
			FromCache bool `json:"from_cache"`
		} `json:"flags"`
		Signals struct {
			SMTPSkippedReason string `json:"smtp_skipped_reason"`
			FailureOrigin     string `json:"failure_origin"`
		} `json:"signals"`
	}
	if err := modclient.Decode(resp, &w, verifyShape); err != nil {
		return Verdict{}, err
	}
	switch {
	case !knownStatus(w.Status):
		return Verdict{}, contractErr(routeVerify, resp, "status", "the status is not one of module 1's four")
	case !knownReason(w.Reason):
		return Verdict{}, contractErr(routeVerify, resp, "reason", "the reason is not one of module 1's vocabulary")
	case w.Confidence < 0 || w.Confidence > 100:
		return Verdict{}, contractErr(routeVerify, resp, "confidence", "the confidence is outside 0..100")
	case !knownRecommendation(w.Recommendation):
		return Verdict{}, contractErr(routeVerify, resp, "recommendation", "the recommendation is not send, review or skip")
	case w.Signals.FailureOrigin != "" && !knownOrigin(w.Signals.FailureOrigin):
		return Verdict{}, contractErr(routeVerify, resp, "signals.failure_origin", "the failure origin is not own, remote or undetermined")
	case w.Signals.SMTPSkippedReason != "" && !knownSkipToken(w.Signals.SMTPSkippedReason):
		return Verdict{}, contractErr(routeVerify, resp, "signals.smtp_skipped_reason", "the SMTP skip token is not one of module 1's")
	}
	return Verdict{
		Status: w.Status, Reason: w.Reason, Confidence: w.Confidence, Recommendation: w.Recommendation, Domain: w.Domain,
		FromCache: w.Flags.FromCache, SkipReason: w.Signals.SMTPSkippedReason, FailureOrigin: w.Signals.FailureOrigin,
	}, nil
}

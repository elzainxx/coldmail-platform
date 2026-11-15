package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// probeJobID is the well-formed (version 4) UUID module 1 is asked about. No job has it, so the answer
// to an authenticated lookup is 404, which is the GOOD answer: authentication accepted, job absent.
const probeJobID = "5ca1ab1e-0000-4000-8000-000000000001"

// requestIDRule is what an error envelope's request_id has to look like. The senders mint req_<32 hex>;
// module 1 mints a UUIDv7, so for module 1 the rule is only "not empty".
type requestIDRule struct {
	want string // completes "expected error.request_id ..."
	ok   func(string) bool
}

var (
	senderRequestID = regexp.MustCompile(`^req_[0-9a-f]{32}$`)
	senderRule      = requestIDRule{"matching ^req_[0-9a-f]{32}$", senderRequestID.MatchString}
	nonEmptyRule    = requestIDRule{"to be non-empty", func(s string) bool { return s != "" }}
)

// envelopeSpec is the exact answer a call must get: a status and an error envelope.
type envelopeSpec struct {
	status     int
	code       string
	statusNote string // appended to a wrong-status reason
	requestID  requestIDRule
}

// recordEnvelope records two checks on one response: <prefix>.<verdict> (the status and error.code are
// what the contract says) and <prefix>.request_id (the envelope carries a well-formed request id). Both
// fail when the request produced no usable response.
func recordEnvelope(rec *Recorder, ex exchange, prefix, verdict string, kind Kind, spec envelopeSpec) {
	verdictID, requestID := prefix+"."+verdict, prefix+".request_id"
	if ex.failure != "" {
		rec.Fail(verdictID, kind, ex.failure)
		rec.Fail(requestID, kind, ex.failure)
		return
	}
	var p problems
	ex.wantStatus(&p, spec.status, spec.statusNote)
	if ex.body == nil {
		p.add("expected a JSON error envelope, got %s", ex.bodyNote)
	} else {
		p.wantString(ex.body, "error.code", spec.code)
	}
	p.verdict(rec, verdictID, kind)

	var q problems
	if ex.body == nil {
		q.add("expected error.request_id %s, got %s instead of an error envelope", spec.requestID.want, ex.bodyNote)
	} else {
		v, found := lookup(ex.body, "error.request_id")
		if s, ok := v.(string); !ok || !spec.requestID.ok(s) {
			q.add("expected error.request_id %s, got %s", spec.requestID.want, show(v, found))
		}
	}
	q.verdict(rec, requestID, kind)
}

// ---- health and readiness (every service) ----------------------------------------------------------------------

func checkHealthz(ctx context.Context, d Doer, rec *Recorder, svc Service) {
	id := svc.Name + ".healthz.ok"
	ex := rec.get(ctx, d, svc, "/healthz", credNone)
	if !ex.usable(rec, id, Positive) {
		return
	}
	var p problems
	ex.wantStatus(&p, 200, "")
	if ex.wantObject(&p) {
		p.wantString(ex.body, "status", "ok")
		p.wantNonEmptyString(ex.body, "at")
	}
	p.verdict(rec, id, Positive)
}

// checkReadyz asserts 200, status "ready", an "at" stamp and every named boolean true. mustBeTrue is the
// per-service list: the checks{} entries and, where the service also carries them, the top-level copies.
func checkReadyz(ctx context.Context, d Doer, rec *Recorder, svc Service, mustBeTrue ...string) {
	id := svc.Name + ".readyz.ok"
	ex := rec.get(ctx, d, svc, "/readyz", credNone)
	if !ex.usable(rec, id, Positive) {
		return
	}
	var p problems
	ex.wantStatus(&p, 200, "")
	if ex.wantObject(&p) {
		p.wantString(ex.body, "status", "ready")
		p.wantNonEmptyString(ex.body, "at")
		for _, path := range mustBeTrue {
			p.wantTrue(ex.body, path)
		}
		if names := notTrueChecks(ex.body); names != "" && len(p) > 0 {
			p.add("checks not true: %s", names)
		}
	}
	p.verdict(rec, id, Positive)
}

// ---- the platform ----------------------------------------------------------------------------------------------

// CheckPlatform checks the platform's own health and readiness (the database ping).
func CheckPlatform(ctx context.Context, d Doer, rec *Recorder, svc Service) {
	checkHealthz(ctx, d, rec, svc)
	checkReadyz(ctx, d, rec, svc, "checks.db", "db")
}

// ---- module 1 (verify) -----------------------------------------------------------------------------------------

// CheckM1 checks module 1: health, readiness, and the job lookup with the caller key (404 not_found:
// authenticated, job absent), without a key and with a wrong key (both 401 unauthenticated). Module 1
// has no describe, which is recorded as NOT-APPLICABLE and is not a pass.
func CheckM1(ctx context.Context, d Doer, rec *Recorder, svc Service) {
	checkHealthz(ctx, d, rec, svc)
	checkReadyz(ctx, d, rec, svc, "checks.pepper", "checks.db", "ready")

	path := "/v1/jobs/" + probeJobID
	// The positive call first: whatever a refused credential does to later calls cannot affect it.
	recordEnvelope(rec, rec.get(ctx, d, svc, path, credConfigured), svc.Name+".jobs.right_key", "accepted", Positive,
		envelopeSpec{404, "not_found", "a 401 means the key was rejected", nonEmptyRule})
	rejected := envelopeSpec{401, "unauthenticated", "", nonEmptyRule}
	recordEnvelope(rec, rec.get(ctx, d, svc, path, credNone), svc.Name+".jobs.no_key", "rejected", Negative, rejected)
	recordEnvelope(rec, rec.get(ctx, d, svc, path, credWrong), svc.Name+".jobs.wrong_key", "rejected", Negative, rejected)

	rec.NotApplicable(svc.Name+".describe",
		"module 1 (verify) has no describe endpoint; its health, readiness and the authenticated-versus-unauthenticated job lookups are checked instead")
}

// ---- modules 2 and 3 (the senders) -----------------------------------------------------------------------------

// sendSpec is what differs between the two senders' describe.
type sendSpec struct {
	module    string // the describe "module" value
	extension bool   // module 3 only: extension_routes, schemas and bulk_add are present (module 2: absent)
}

// CheckM2 checks module 2 (send_managed).
func CheckM2(ctx context.Context, d Doer, rec *Recorder, svc Service) {
	checkSender(ctx, d, rec, svc, sendSpec{module: "send_managed"})
}

// CheckM3 checks module 3 (send_byo).
func CheckM3(ctx context.Context, d Doer, rec *Recorder, svc Service) {
	checkSender(ctx, d, rec, svc, sendSpec{module: "send_byo", extension: true})
}

func checkSender(ctx context.Context, d Doer, rec *Recorder, svc Service, spec sendSpec) {
	checkHealthz(ctx, d, rec, svc)
	checkReadyz(ctx, d, rec, svc, "checks.db", "checks.singleton_lock", "db", "singleton_lock")

	// The positive call first, then the two refusals that prove the positive can fail.
	checkDescribe(rec, rec.get(ctx, d, svc, "/v1/describe", credConfigured), svc.Name, spec)
	rejected := envelopeSpec{401, "unauthenticated", "", senderRule}
	recordEnvelope(rec, rec.get(ctx, d, svc, "/v1/describe", credNone), svc.Name+".describe.no_token", "rejected", Negative, rejected)
	recordEnvelope(rec, rec.get(ctx, d, svc, "/v1/describe", credWrong), svc.Name+".describe.wrong_token", "rejected", Negative, rejected)
}

// checkDescribe records describe.ok and one check per asserted field. When the describe call produced no
// usable body every field check fails with that reason: an absent field cannot be proved absent in an
// error body, so nothing here passes by omission.
func checkDescribe(rec *Recorder, ex exchange, name string, spec sendSpec) {
	okID := name + ".describe.ok"
	if ex.usable(rec, okID, Positive) {
		var p problems
		ex.wantStatus(&p, 200, "")
		ex.wantObject(&p)
		p.verdict(rec, okID, Positive)
	}

	unusable := ""
	switch {
	case ex.failure != "":
		unusable = ex.failure
	case ex.status != 200:
		unusable = fmt.Sprintf("describe answered %d, not 200, so this field cannot be checked", ex.status)
	case ex.body == nil:
		unusable = "describe body is not a JSON object, so this field cannot be checked"
	}
	field := func(suffix string, fn func(p *problems, m map[string]any)) {
		id := name + ".describe." + suffix
		if unusable != "" {
			rec.Fail(id, Positive, unusable)
			return
		}
		var p problems
		fn(&p, ex.body)
		p.verdict(rec, id, Positive)
	}

	field("module", func(p *problems, m map[string]any) { p.wantString(m, "module", spec.module) })
	field("api_version", func(p *problems, m map[string]any) { p.wantString(m, "api_version", "v1") })
	field("capabilities", func(p *problems, m map[string]any) { p.wantObjectAt(m, "capabilities") })
	field("pricing", func(p *problems, m map[string]any) {
		p.wantObjectAt(m, "pricing")
		p.wantString(m, "pricing.currency", "credits")
	})
	// A non-empty job_state proves the module read its database enums (they are read live from pg_enum).
	field("vocabularies", func(p *problems, m map[string]any) { p.wantNonEmptyArray(m, "vocabularies.job_state.values") })
	field("error_codes", wantErrorCodes)

	// A recorded difference between the modules, asserted in both directions: module 3 carries these
	// three keys and module 2 does not.
	for _, key := range []string{"extension_routes", "schemas", "bulk_add"} {
		if spec.extension {
			field("present."+key, func(p *problems, m map[string]any) {
				v, found := lookup(m, key)
				switch {
				case key == "extension_routes":
					p.wantNonEmptyArray(m, key)
				case !found || v == nil:
					p.add("expected %s to be present, got %s", key, show(v, found))
				}
			})
		} else {
			field("absent."+key, func(p *problems, m map[string]any) {
				if v, found := lookup(m, key); found {
					p.add("expected %s to be absent in module 2, got %s", key, show(v, found))
				}
			})
		}
	}
}

// wantErrorCodes asserts error_codes is a non-empty array of {code, http_status}.
func wantErrorCodes(p *problems, m map[string]any) {
	v, found := lookup(m, "error_codes")
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		p.add("expected error_codes to be a non-empty array, got %s", show(v, found))
		return
	}
	for i, e := range arr {
		obj, _ := e.(map[string]any)
		code, hasCode := obj["code"]
		status, hasStatus := obj["http_status"]
		s, _ := code.(string)
		n, isNum := status.(json.Number)
		st, err := n.Int64()
		if s == "" || !isNum || err != nil || st < 100 || st > 599 {
			p.add("expected error_codes[%d] to be {code: non-empty string, http_status: 100-599}, got code=%s http_status=%s",
				i, show(code, hasCode), show(status, hasStatus))
			return
		}
	}
}

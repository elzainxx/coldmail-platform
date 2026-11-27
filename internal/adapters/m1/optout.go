package m1

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// optout.go: the per-user vendor opt-out as module 1 serves it (Phase 5 S6; module 1's ADR-0024, Docs/api-contract.md "Policies"). Module 1 serves a SECOND policy, the default's twin without the
// `external` block, so a bulk job created under it can never reach the paid vendor whatever the operator's switches say. Three facts of the contract are used here and nothing else:
//
//	where the key is        GET /v1/describe, vendor_fallback.opt_out_policy: the name of the second policy, present exactly when the module serves it, whatever vendor_fallback.reachable says. The platform READS it
//	                        (OptOutPolicy, OptOutPolicyOf) and holds no constant for it: module 1's describe is the one source of truth. A module that does not publish it (an older module 1, or one that could
//	                        not seed the record) is Known false and refuses the name: the platform never submits an opted-out job without it.
//	how a job is put there  the `policy` member of POST /v1/verify/bulk (SubmitUnder).
//	what a result says      every result row carries the policy it was made under in meta.policy and meta.policy_version (a cache row too: a vendor-derived cache entry may answer an opted-out job, and the row says
//	                        the opt-out policy and from_cache). ResultsUnder checks it.

// ErrNotUnderPolicy is the cause of the Contract error ResultsUnder returns for a page that holds a row not stamped with the policy the job was submitted under. It is a violation of the contract the
// platform relied on to keep a customer's choice (the job was told not to use the vendor and a row says it was made under another policy): the page is refused whole, the rows are not recorded, and
// the engine flags the job and waits (internal/jobengine counts it as an answer it does not understand).
var ErrNotUnderPolicy = errors.New("m1: a result is not stamped with the policy the job was submitted under")

// OptOutPolicy is module 1's answer to "which policy opts a job out of the vendor?", as an explicit pair: a module whose describe does not carry the key is Known false, never a silent empty key. Key is
// meaningful only while Known, and then it is a valid policy name (ValidPolicyKey).
type OptOutPolicy struct {
	Known bool
	Key   string
}

// OptOutPolicy reads module 1's describe (Describe, with every check it makes) and returns what it says about its opt-out policy. A describe without the key is {Known: false}, not an error. A module that
// cannot be asked is the typed modclient error of the call, and ON ANY ERROR THE RESULT IS THE ZERO VALUE: an error is never a Known answer.
func (a *Adapter) OptOutPolicy(ctx context.Context) (OptOutPolicy, error) {
	d, err := a.Describe(ctx)
	if err != nil {
		return OptOutPolicy{}, err
	}
	return OptOutPolicyOf(d)
}

// optOutDoc is the one key of the describe OptOutPolicyOf reads. A pointer says "absent"; the describe's shape has already refused a wrong type before this is decoded.
type optOutDoc struct {
	VendorFallback *struct {
		OptOutPolicy *string `json:"opt_out_policy"`
	} `json:"vendor_fallback"`
}

// OptOutPolicyOf reads vendor_fallback.opt_out_policy out of a describe the adapter's Describe returned. Absent (no vendor_fallback, or no key in it) is {Known: false}. A key that is not a policy name
// (empty, too long, or holding characters a name never has) is a Contract error naming vendor_fallback.opt_out_policy: the platform would otherwise put it into a request body. (A key of the wrong TYPE is
// refused earlier, by Describe's shape, which names the same key; a string that is not a name is refused here only, so the vendor's reachability, read by VendorReachableOf from the same describe, stays readable.)
func OptOutPolicyOf(d *modclient.Describe) (OptOutPolicy, error) {
	if d == nil {
		return OptOutPolicy{}, newErr(modclient.Invalid, routeDescribe, "there is no describe to read the opt-out policy from")
	}
	var v optOutDoc
	if err := json.Unmarshal(d.Raw, &v); err != nil {
		return OptOutPolicy{}, vendorContract(d, "vendor_fallback.opt_out_policy", "the describe does not fit the type the adapter reads the opt-out policy with")
	}
	if v.VendorFallback == nil || v.VendorFallback.OptOutPolicy == nil {
		return OptOutPolicy{}, nil
	}
	key := *v.VendorFallback.OptOutPolicy
	if !ValidPolicyKey(key) {
		return OptOutPolicy{}, vendorContract(d, "vendor_fallback.opt_out_policy", "the describe publishes an opt-out policy that is not a policy name")
	}
	return OptOutPolicy{Known: true, Key: key}, nil
}

// checkPolicy is ResultsUnder's judgement of a decoded page: every row must say it was made under want, and under a version (a positive number: every work item stamps one). The first row that does not is
// a Contract error naming results[i].meta.policy (or .policy_version): the key, never the value that was there.
func checkPolicy(resp *modclient.Response, rows []Row, want string) error {
	for i, r := range rows {
		at := "results[" + strconv.Itoa(i) + "].meta."
		switch {
		case r.Policy != want:
			e := contractErr(routeResults, resp, at+"policy", "a result is not stamped with the policy the job was submitted under (the job's opt-out is not being honoured, or module 1 stopped saying which policy made the row)")
			e.Cause = ErrNotUnderPolicy
			return e
		case r.PolicyVersion < 1:
			e := contractErr(routeResults, resp, at+"policy_version", "a result of a job submitted under a named policy carries no policy version")
			e.Cause = ErrNotUnderPolicy
			return e
		}
	}
	return nil
}

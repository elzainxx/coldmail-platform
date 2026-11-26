package m1

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// vendor.go: what module 1's describe says about its paid vendor fallback (module 1 migration 0020, owner ruling C3, ADR-0023): `vendor_fallback: {"reachable": bool}`, true only when EVERY switch the
// external-fallback stage itself requires is on at this moment (the fallback rung, the DPA confirmation, the vendor key present in the variable the key dial names, a daily spend cap above zero, the
// policy opting in, the kill switch not pulled). The module computes it per request from the one predicate its stage is wired from, so the answer and the stage cannot disagree; it says whether and
// never how. The platform reads it before it previews or confirms a module 1 job: reachable means a job may be charged the vendor's price, which is what the reserve must cover.

// vendorFallbackShape is the object `vendor_fallback`: `reachable` is a required boolean; a key module 1 adds to it later is tolerated (and not a difference for the drift gate).
var vendorFallbackShape = modclient.Object(modclient.Req("reachable", modclient.Bool)).Open()

// VendorReachable is module 1's answer to "can a vendor answer happen now?", as an explicit tri-state: a module whose describe does not carry the field (every module 1 before migration 0020)
// is Known false, NEVER a silent Reachable false. Reachable means nothing while Known is false (it is the zero value, false, and
// no caller may read it: every caller in this repository asks Known first, and the reserve's rule for an unknown answer is in internal/jobs, never a default here).
type VendorReachable struct {
	// Known is true when the describe carried vendor_fallback.reachable.
	Known bool
	// Reachable is the module's answer; meaningful only when Known.
	Reachable bool
}

// VendorReachable reads module 1's describe (Describe, with every check it makes: the module is module 1, offers what the adapter relies on, and the vendor_fallback it carries, if any, has its
// shape) and returns what it says about the vendor fallback. A describe without the field is the answer {Known: false}, not an error. A module that cannot be asked (down, slow, refusing the key,
// paused) is the typed modclient error of the call and ON ANY ERROR THE RESULT IS THE ZERO VALUE: an error is never a Reachable false.
func (a *Adapter) VendorReachable(ctx context.Context) (VendorReachable, error) {
	d, err := a.Describe(ctx)
	if err != nil {
		return VendorReachable{}, err
	}
	return VendorReachableOf(d)
}

// vendorDoc is the one key of the describe VendorReachableOf reads. A pointer says "absent"; the shape has already refused null and a wrong type before this is decoded.
type vendorDoc struct {
	VendorFallback *struct {
		Reachable *bool `json:"reachable"`
	} `json:"vendor_fallback"`
}

// VendorReachableOf reads vendor_fallback.reachable out of a describe the adapter's Describe returned (d.Raw is the body, already judged by describeShape). Absent is {Known: false}. A document
// that holds the object without the boolean (a describe not made by Describe) is a Contract error naming vendor_fallback.reachable: never a guess.
func VendorReachableOf(d *modclient.Describe) (VendorReachable, error) {
	if d == nil {
		return VendorReachable{}, newErr(modclient.Invalid, routeDescribe, "there is no describe to read the vendor fallback from")
	}
	var v vendorDoc
	if err := json.Unmarshal(d.Raw, &v); err != nil {
		return VendorReachable{}, vendorContract(d, "vendor_fallback", "the describe does not fit the type the adapter reads the vendor fallback with")
	}
	switch {
	case v.VendorFallback == nil:
		return VendorReachable{}, nil
	case v.VendorFallback.Reachable == nil:
		return VendorReachable{}, vendorContract(d, "vendor_fallback.reachable", "the describe carries a vendor_fallback with no reachable flag")
	}
	return VendorReachable{Known: true, Reachable: *v.VendorFallback.Reachable}, nil
}

func vendorContract(d *modclient.Describe, key, msg string) error {
	e := newErr(modclient.Contract, routeDescribe, msg)
	e.Key, e.Sent, e.Status, e.RequestID = key, true, http.StatusOK, d.RequestID
	return e
}

package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The opt-out policy key of module 1 (Phase 5 S6, reachability_provider.go: M1OptOutPolicy) over the REAL module 1 adapter and a stand-in of its describe. The bodies are HAND-WRITTEN from module 1's
// Docs/api-contract.md at 8aa147c: the adapter's recorded describe plus the one key module 1 adds (`vendor_fallback.opt_out_policy`), which the lead re-records after the pin; the tests hold before and after.

const appNoVendorKey = "cold-email-no-vendor"

func TestM1Reachability_TheOptOutPolicyIsTheKeyModuleOneSaysAndNothingElse(t *testing.T) {
	key, err := tenant.DeriveM1KeyVersion(config.Secret(providerSecretText), 3, m1ServiceCallerID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		vendor map[string]any
		want   string
		err    error
	}{
		{"the key, the vendor not reachable (hand-written)", map[string]any{"reachable": false, "opt_out_policy": appNoVendorKey}, appNoVendorKey, nil},
		{"the key, the vendor reachable (hand-written)", map[string]any{"reachable": true, "opt_out_policy": appNoVendorKey}, appNoVendorKey, nil},
		{"another name, said by module 1: the platform holds no name of its own", map[string]any{"reachable": false, "opt_out_policy": "some-other-policy"}, "some-other-policy", nil},
		{"module 1 says reachable and publishes no key", map[string]any{"reachable": true}, "", jobs.ErrNoOptOutPolicy},
		{"an older module 1: no vendor_fallback at all", nil, "", jobs.ErrNoOptOutPolicy},
	} {
		ens := &recordingEnsurer{}
		srv := newModuleOneStandIn(t, key.Reveal(), describeWith(t, c.vendor))
		p, _ := providerOver(t, srv, ens)
		got, err := p.M1OptOutPolicy(t.Context())
		if got != c.want || !errors.Is(err, c.err) || (c.err == nil && err != nil) {
			t.Errorf("%s: read (%q, %v), want (%q, %v)", c.name, got, err, c.want, c.err)
		}
		reqs, auth := srv.seen()
		if len(reqs) != 1 || reqs[0] != "GET /v1/describe" || auth[0] != "Bearer "+key.Reveal() {
			t.Errorf("%s: the stand-in saw %v: want one GET /v1/describe as the service caller", c.name, reqs)
		}
		if got := ens.asked(); len(got) != 1 || got[0] != m1ServiceCallerID {
			t.Errorf("%s: the service caller was registered through %v", c.name, got)
		}
	}
}

func TestM1Reachability_AnOptOutKeyIsNeverMadeUpFromAFailureOrAMalformedDescribe(t *testing.T) {
	key, _ := tenant.DeriveM1KeyVersion(config.Secret(providerSecretText), 3, m1ServiceCallerID)

	// a malformed key (not a policy name) is an error that names the key and carries none of it
	srv := newModuleOneStandIn(t, key.Reveal(), describeWith(t, map[string]any{"reachable": false, "opt_out_policy": "cold email/no vendor"}))
	p, _ := providerOver(t, srv, &recordingEnsurer{})
	got, err := p.M1OptOutPolicy(t.Context())
	if got != "" || err == nil || errors.Is(err, jobs.ErrNoOptOutPolicy) || strings.Contains(err.Error(), "no vendor") {
		t.Errorf("a malformed key read (%q, %v), want no key and an error that is not 'publishes none' and quotes nothing", got, err)
	}

	// a failed registration: no key, and module 1 is never asked
	srv2 := newModuleOneStandIn(t, key.Reveal(), describeWith(t, map[string]any{"reachable": false, "opt_out_policy": appNoVendorKey}))
	p2, _ := providerOver(t, srv2, &recordingEnsurer{fail: errors.New("the database said no")})
	got, err = p2.M1OptOutPolicy(t.Context())
	if got != "" || err == nil || !strings.Contains(err.Error(), "service caller") {
		t.Errorf("a failed registration read (%q, %v), want no key and an error naming the service caller", got, err)
	}
	if reqs, _ := srv2.seen(); len(reqs) != 0 {
		t.Errorf("module 1 was asked %d time(s) after the registration failed", len(reqs))
	}

	// a key of another version is refused by module 1: an error of the call, never a key
	srv3 := newModuleOneStandIn(t, key.Reveal(), describeWith(t, map[string]any{"reachable": false, "opt_out_policy": appNoVendorKey}))
	p3, _ := providerOver(t, srv3, &recordingEnsurer{})
	p3.version = 4
	got, err = p3.M1OptOutPolicy(t.Context())
	if got != "" || err == nil || modclient.KindOf(err) != modclient.Refused {
		t.Errorf("a key of another version read (%q, %v), want a Refused error and no key", got, err)
	}
}

//go:build harness

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	m1 "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The commands and flags of stage J7 (tests/harness/jobs/stage.sh, jobs_j7; Phase 5 S6, the per-user vendor opt-out): two users run the SAME kind of list through the real platform, the real module 1 and module
// 1's own ZeroBounce stand-in (its test/zbstub, which counts every request it gets); one is opted out of the vendor fallback, the other is not. What is read, from places that cannot agree by accident:
//
//	create/setup --optout             the user states the setting through the API (PUT /v1/settings) before the job is created, so the job reads it when it is created
//	create/setup --vendor-tag T       the list is made of addresses at a domain whose MX the fixture DNS says is a big provider (module 1 settles it as risky/provider_no_smtp, the class it routes to the
//	                                  vendor when the vendor can be reached); the local part is `valid+T<n>`, which the stand-in answers `valid` (module 1 then says risky/external_verified)
//	policy --job J [--expect ...]     module 1's OWN results of the job, read with the user's derived key: how many rows, the policies they say they were made under, whether module 1 publishes its opt-out
//	                                  key and whether every row is under it, how many are vendor answers and how many are the routed provider's
//	stub [--expect total=N]           how many requests the vendor stand-in has received in all

// vendorDomain is the domain of the J7 lists: its MX is a Google host in the fixture DNS (tests/harness/jobs/dns.json), which module 1's policy routes to the vendor stage as provider_no_smtp.
const vendorDomain = "provider.example.test"

// addOptOutFlags declares the two flags of stage J7 on a create or a setup.
func addOptOutFlags(fs *flag.FlagSet, o *createOpts) {
	fs.BoolVar(&o.optOut, "optout", false, "the user opts out of the vendor fallback (PUT /v1/settings) before the job is created")
	fs.StringVar(&o.vendorTag, "vendor-tag", "", "a module 1 job whose good addresses are valid+<tag><n>@"+vendorDomain+" (routed to the vendor stage; the stand-in answers valid)")
}

// vendorLines is the list of a --vendor-tag job.
func vendorLines(o createOpts) ([]string, error) {
	return genRowsWith(o.rows, o.bad, o.dup, func(i int) string { return fmt.Sprintf("valid+%s%05d@%s", o.vendorTag, i, vendorDomain) }, "")
}

// stateOptOut makes the user state the opt-out through the API, as the user, and requires the answer to say it is now in force.
func stateOptOut(ctx context.Context, sg *signer, api, user string) error {
	a, err := call(ctx, httpClient(time.Minute), api, http.MethodPut, "/v1/settings", sg.mint(user, time.Now()), nil, map[string]any{"vendor_fallback_opt_out": true})
	if err != nil {
		return err
	}
	var got struct {
		Optout *bool `json:"vendor_fallback_opt_out"`
	}
	if a.Status != http.StatusOK || json.Unmarshal(a.Body, &got) != nil || got.Optout == nil || !*got.Optout {
		return failf(exitRefused, "stating the opt-out: the platform answered %d %s (request %s), want 200 and vendor_fallback_opt_out true", a.Status, orDash(a.ErrorCode), orDash(a.RequestID))
	}
	fact("optout", "user", user, "vendor_fallback_opt_out", true)
	return nil
}

// ---- policy -----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func cmdPolicy(args []string) error {
	fs := flags("policy")
	var job, expect string
	fs.StringVar(&job, "job", "", "the job")
	fs.StringVar(&expect, "expect", "", "conditions on rows, policies, key_known, all_under_key, external_verified, provider_no_smtp")
	if err := parse(fs, args); err != nil {
		return err
	}
	if job == "" {
		return failf(exitUsage, "--job is required")
	}
	conds, err := parseConditions(expect)
	if err != nil {
		return err
	}
	ctx, cancel := ctxFor(2 * time.Minute)
	defer cancel()
	d, err := connect(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	h, err := d.head(ctx, job)
	if err != nil {
		return err
	}
	var pending int
	if err := d.c.QueryRow(ctx, `SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid AND state = 'pending'`, job).Scan(&pending); err != nil {
		return d.scrub(err)
	}
	report, problems := moduleReport(ctx, h, pending) // verify.go: the user's own key, the job's status and totals, every page of its results
	if len(problems) > 0 {
		return failf(exitFail, "module 1's report of job %s: %s", job, strings.Join(problems, "; "))
	}
	key, known, err := optOutKeyOf(ctx, h.User)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	allUnder, vendor, routed := known, 0, 0
	for _, r := range report {
		seen[r.Policy] = true
		allUnder = allUnder && r.Policy == key
		switch r.Reason {
		case "external_verified":
			vendor++
		case "provider_no_smtp":
			routed++
		}
	}
	var pols []string
	for p := range seen {
		pols = append(pols, p)
	}
	sort.Strings(pols)
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	s := factOf(kv{"rows", len(report)}, kv{"policies", strings.Join(pols, "+")}, kv{"key_known", b2i(known)}, kv{"all_under_key", b2i(allUnder)}, kv{"external_verified", vendor}, kv{"provider_no_smtp", routed})
	fact("policy", append([]any{"job", job}, factPairs(s)...)...)
	return judge("policy", conds, s)
}

// optOutKeyOf is the key module 1's own describe publishes as vendor_fallback.opt_out_policy, asked with the user's derived key (any caller may read it).
func optOutKeyOf(ctx context.Context, user string) (string, bool, error) {
	secret, err := env("JOBS_M1_KEY_SECRET")
	if err != nil {
		return "", false, err
	}
	version := 1
	if v := envOr("JOBS_M1_KEY_VERSION", "1"); v != "1" {
		n, perr := strconv.Atoi(v)
		if perr != nil {
			return "", false, failf(exitFail, "JOBS_M1_KEY_VERSION is not a number")
		}
		version = n
	}
	derived, err := tenant.DeriveM1KeyVersion(config.Secret(secret), version, user)
	if err != nil {
		return "", false, failf(exitFail, "the user's module 1 key cannot be derived: %v", err)
	}
	ad, err := m1.New(envOr("JOBS_M1_URL", "http://m1:8080"), derived, nil, modclient.Options{Timeout: 30 * time.Second, Retries: 2, Backoff: 250 * time.Millisecond})
	if err != nil {
		return "", false, failf(exitFail, "the module 1 client cannot be built: %v", err)
	}
	o, err := ad.OptOutPolicy(ctx)
	if err != nil {
		return "", false, failf(exitFail, "module 1's describe cannot be read: %v", err)
	}
	return o.Key, o.Known, nil
}

// ---- stub -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func cmdStub(args []string) error {
	fs := flags("stub")
	var url, expect string
	fs.StringVar(&url, "url", "http://zbstub:8080", "the vendor stand-in (module 1's test/zbstub, service zbstub of the compose file)")
	fs.StringVar(&expect, "expect", "", "conditions on total")
	if err := parse(fs, args); err != nil {
		return err
	}
	conds, err := parseConditions(expect)
	if err != nil {
		return err
	}
	ctx, cancel := ctxFor(time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/__calls", nil)
	if err != nil {
		return failf(exitFail, "cannot build the request to the stand-in: %v", err)
	}
	resp, err := httpClient(30 * time.Second).Do(req)
	if err != nil {
		return failf(exitNoResponse, "the vendor stand-in did not answer: %v", clip(err.Error(), 160))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var got struct {
		Total *int `json:"total"`
	}
	if err != nil || resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &got) != nil || got.Total == nil {
		return failf(exitFail, "the vendor stand-in's counters are not readable (status %d)", resp.StatusCode)
	}
	s := factOf(kv{"total", *got.Total})
	fact("stub", factPairs(s)...)
	return judge("stub", conds, s)
}

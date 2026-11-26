//go:build harness

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	m1 "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The commands of stage J6 (tests/harness/jobs/stage.sh, jobs_j6): module 1's vendor fallback is switched off, on and off again by re-creating its container, and what the PLATFORM does with each
// answer is read from four places that cannot agree by accident: module 1's own describe (`vendor`), the platform's stored flag and its history (`flag`), the preview the API answers (`preview`) and
// the ledger and the price snapshot of a confirmed job (`reserve`). Every one prints one fact line and may be given --expect conditions, the syntax of `state` (name=value, name>n, ...: a condition on a
// name the fact does not have is a usage failure, never a pass).

// serviceCallerID is the id of the platform's own caller at module 1 (internal/app.m1ServiceCallerID): the key `vendor` asks module 1 with is the one the platform derives for it. The scope's selftest
// holds this line equal to the platform's.
const serviceCallerID = "00000000-0000-4000-8000-00000000d5c1"

// factOf builds the snapshot a fact line is judged as: the conditions of --expect read its members by name.
func factOf(vals ...kv) snapshot { return snapshot{Vals: vals} }

// judge asserts the conditions against a fact and says which were not met.
func judge(what string, conds []condition, s snapshot) error {
	var unmet []string
	for _, c := range conds {
		ok, got, err := c.holds(s)
		if err != nil {
			return err
		}
		if !ok {
			unmet = append(unmet, fmt.Sprintf("%s%s%s (it is %s)", c.name, c.op, c.value, got))
		}
	}
	if len(unmet) > 0 {
		return failf(exitFail, "%s does not hold: %s", what, strings.Join(unmet, "; "))
	}
	if len(conds) > 0 {
		fact(what+"_holds", "conditions", len(conds))
	}
	return nil
}

func factPairs(s snapshot) []any {
	var out []any
	for _, p := range s.Vals {
		out = append(out, p.Name, p.Value)
	}
	return out
}

// ---- vendor: what module 1 itself says -----------------------------------------------------------------------------------------------------------------------------------------------

// cmdVendor asks module 1's describe, as the platform's service caller, and prints `vendor_fallback.reachable` (known=0 when the describe has none). --wait-registered S first waits up to S seconds
// for the platform to have registered that caller at module 1 (it does so by itself, at its first read): a caller that never appears is a failure naming it, so a platform that cannot read module 1
// is not mistaken for a module that cannot be read.
func cmdVendor(args []string) error {
	fs := flags("vendor")
	var expect string
	var wait int
	fs.StringVar(&expect, "expect", "", "reachable=true|false, known=0|1 conditions, comma separated")
	fs.IntVar(&wait, "wait-registered", 0, "seconds to wait for the platform to have registered its service caller at module 1")
	if err := parse(fs, args); err != nil {
		return err
	}
	conds, err := parseConditions(expect)
	if err != nil {
		return err
	}
	ctx, cancel := ctxFor(3 * time.Minute)
	defer cancel()
	if wait > 0 {
		d, err := connect(ctx)
		if err != nil {
			return err
		}
		defer d.close()
		deadline := time.Now().Add(time.Duration(wait) * time.Second)
		for {
			var n int
			if err := d.c.QueryRow(ctx, `SELECT count(*) FROM verify.api_callers WHERE caller_id = $1`, tenant.M1CallerID(serviceCallerID)).Scan(&n); err != nil {
				return d.scrub(err)
			}
			if n == 1 {
				break
			}
			if time.Now().After(deadline) {
				return failf(exitFail, "the platform's service caller (%s) is not registered at module 1 %d s after the platform started: the platform has not read module 1's describe, or could not register its key", tenant.M1CallerID(serviceCallerID), wait)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	secret, err := env("JOBS_M1_KEY_SECRET")
	if err != nil {
		return err
	}
	version := 1
	if v := envOr("JOBS_M1_KEY_VERSION", "1"); v != "1" {
		n, perr := strconv.Atoi(v)
		if perr != nil {
			return failf(exitFail, "JOBS_M1_KEY_VERSION is not a number")
		}
		version = n
	}
	key, err := tenant.DeriveM1KeyVersion(config.Secret(secret), version, serviceCallerID)
	if err != nil {
		return failf(exitFail, "the service caller's key cannot be derived: %v", err)
	}
	ad, err := m1.New(envOr("JOBS_M1_URL", "http://m1:8080"), key, nil, modclient.Options{Timeout: 20 * time.Second, Retries: 2, Backoff: 250 * time.Millisecond})
	if err != nil {
		return failf(exitFail, "the module 1 client cannot be built: %v", err)
	}
	v, err := ad.VendorReachable(ctx)
	if err != nil {
		return failf(exitFail, "module 1's describe cannot be read: %v", err)
	}
	s := factOf(kv{"known", b2i(v.Known)}, kv{"reachable", fmt.Sprint(v.Reachable)})
	fact("vendor", factPairs(s)...)
	return judge("vendor", conds, s)
}

// ---- flag: what the platform has stored ---------------------------------------------------------------------------------------------------------------------------------------------

// cmdFlag prints the platform's stored reachable flag of m1_vendor, who set it, and its history: the number of rows, the latest row and how many were written by module 1's answer.
func cmdFlag(args []string) error {
	fs := flags("flag")
	var expect string
	fs.StringVar(&expect, "expect", "", "conditions on reachable, source, history, last_reachable, last_source, from_module1")
	if err := parse(fs, args); err != nil {
		return err
	}
	conds, err := parseConditions(expect)
	if err != nil {
		return err
	}
	ctx, cancel := ctxFor(time.Minute)
	defer cancel()
	d, err := connect(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	var reach bool
	var source string
	var hist, fromModule int64
	var lastReach bool
	var lastSource string
	err = d.c.QueryRow(ctx, `SELECT u.reachable, COALESCE(u.reachable_source, ''),
			(SELECT count(*) FROM platform.unit_reachability_history WHERE unit = 'm1_vendor'),
			(SELECT count(*) FROM platform.unit_reachability_history WHERE unit = 'm1_vendor' AND source = 'm1_describe'),
			COALESCE((SELECT reachable FROM platform.unit_reachability_history WHERE unit = 'm1_vendor' ORDER BY id DESC LIMIT 1), false),
			COALESCE((SELECT source FROM platform.unit_reachability_history WHERE unit = 'm1_vendor' ORDER BY id DESC LIMIT 1), '')
		FROM platform.unit_costs u WHERE u.unit = 'm1_vendor'`).Scan(&reach, &source, &hist, &fromModule, &lastReach, &lastSource)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return failf(exitFail, "platform.unit_costs has no row m1_vendor")
		}
		return d.scrub(err)
	}
	s := factOf(kv{"reachable", fmt.Sprint(reach)}, kv{"source", source}, kv{"history", hist}, kv{"from_module1", fromModule}, kv{"last_reachable", fmt.Sprint(lastReach)}, kv{"last_source", lastSource})
	fact("flag", factPairs(s)...)
	return judge("flag", conds, s)
}

// ---- preview: what the API says the list will cost -----------------------------------------------------------------------------------------------------------------------------------

// cmdPreview previews (or re-previews) a job through the platform's API, as its user, and prints the numbers of the preview: the rate the reserve would be made at, the credits, and whether module 1
// was read for it.
func cmdPreview(args []string) error {
	fs := flags("preview")
	var api, job, expect string
	fs.StringVar(&api, "api", "http://jobs-a:8080", "the platform instance to call")
	fs.StringVar(&job, "job", "", "the draft or previewed job")
	fs.StringVar(&expect, "expect", "", "conditions on unit_cost, rate_unit, credits, units, pending, known")
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
	ctx, cancel := ctxFor(3 * time.Minute)
	defer cancel()
	sg, err := newSigner()
	if err != nil {
		return err
	}
	d, err := connect(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	h, err := d.head(ctx, job)
	if err != nil {
		return err
	}
	a, err := call(ctx, httpClient(90*time.Second), api, http.MethodPost, "/v1/jobs/"+job+"/preview", sg.mint(h.User, time.Now()), nil, nil)
	if err != nil {
		return err
	}
	if a.Status != http.StatusOK {
		return refused("previewing the job", a)
	}
	var body struct {
		Job struct {
			Status  string `json:"status"`
			Preview *struct {
				Pending     int    `json:"pending"`
				Units       int    `json:"units"`
				Metered     bool   `json:"metered"`
				RateUnit    string `json:"rate_unit"`
				UnitCost    int    `json:"unit_cost"`
				Credits     int64  `json:"credits"`
				Known       *bool  `json:"vendor_reachable_known"`
				Fingerprint string `json:"fingerprint"`
			} `json:"preview"`
		} `json:"job"`
	}
	if err := json.Unmarshal(a.Body, &body); err != nil || body.Job.Preview == nil || len(body.Job.Preview.Fingerprint) != 64 {
		return failf(exitFail, "the platform's answer to the preview is not a previewed job with a fingerprint (status %d, request %s)", a.Status, orDash(a.RequestID))
	}
	p := body.Job.Preview
	if p.Known == nil {
		return failf(exitFail, "the preview has no vendor_reachable_known: the platform does not say whether module 1 was read (a preview must always carry it, true or false)")
	}
	s := factOf(kv{"status", body.Job.Status}, kv{"pending", p.Pending}, kv{"units", p.Units}, kv{"metered", b2i(p.Metered)}, kv{"rate_unit", p.RateUnit},
		kv{"unit_cost", p.UnitCost}, kv{"credits", p.Credits}, kv{"known", b2i(*p.Known)}, kv{"fingerprint", p.Fingerprint})
	fact("preview", append([]any{"job", job}, factPairs(s)...)...)
	return judge("preview", conds, s)
}

// ---- reserve: what a job holds ------------------------------------------------------------------------------------------------------------------------------------------------------

// cmdReserve prints what the ledger and the job's price snapshot hold for a job: the reserve row (its unit, units, frozen unit cost, credits), the units of the snapshot (and the price of the vendor
// unit in it, 0 when it is not part of it), the reserve's running totals and the user's balance. It writes nothing.
func cmdReserve(args []string) error {
	fs := flags("reserve")
	var job, expect string
	fs.StringVar(&job, "job", "", "the job")
	fs.StringVar(&expect, "expect", "", "conditions on reserve_rows, rate_unit, units, unit_cost, reserved_credits, snapshot_units, snapshot_vendor, reserved, charged, released, remainder, release_rows, balance_reserved")
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
	ctx, cancel := ctxFor(time.Minute)
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
	s, err := d.reserveFact(ctx, h)
	if err != nil {
		return err
	}
	fact("reserve", append([]any{"job", job, "status", h.Status}, factPairs(s)...)...)
	return judge("reserve", conds, s)
}

// reserveFact reads the numbers of `reserve` in one statement over the job.
func (d *db) reserveFact(ctx context.Context, h jobHead) (snapshot, error) {
	var rows, units, unitCost, credits, snapUnits, snapVendor, reserved, charged, released, releaseRows, balReserved, pending int64
	var rateUnit string
	err := d.c.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'reserve'),
		COALESCE((SELECT unit FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'reserve'), ''),
		COALESCE((SELECT units FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'reserve'), 0),
		COALESCE((SELECT unit_cost FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'reserve'), 0),
		COALESCE((SELECT reserved_delta FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'reserve'), 0),
		(SELECT count(*) FROM platform.job_unit_prices WHERE job_id = $1::uuid),
		COALESCE((SELECT cost FROM platform.job_unit_prices WHERE job_id = $1::uuid AND unit = 'm1_vendor'), 0),
		COALESCE((SELECT reserved FROM platform.reserves WHERE job_id = $1::uuid), 0),
		COALESCE((SELECT charged FROM platform.reserves WHERE job_id = $1::uuid), 0),
		COALESCE((SELECT released FROM platform.reserves WHERE job_id = $1::uuid), 0),
		(SELECT count(*) FROM platform.ledger WHERE job_id = $1::uuid AND kind = 'release'),
		COALESCE((SELECT reserved FROM platform.balances WHERE user_id = $2::uuid AND module_id = 'm1'), 0),
		(SELECT count(*) FROM platform.job_rows WHERE job_id = $1::uuid AND state = 'pending')`, h.ID, h.User).
		Scan(&rows, &rateUnit, &units, &unitCost, &credits, &snapUnits, &snapVendor, &reserved, &charged, &released, &releaseRows, &balReserved, &pending)
	if err != nil {
		return snapshot{}, d.scrub(err)
	}
	return factOf(kv{"reserve_rows", rows}, kv{"rate_unit", rateUnit}, kv{"units", units}, kv{"unit_cost", unitCost}, kv{"reserved_credits", credits},
		kv{"snapshot_units", snapUnits}, kv{"snapshot_vendor", snapVendor}, kv{"reserved", reserved}, kv{"charged", charged}, kv{"released", released},
		kv{"remainder", reserved - charged - released}, kv{"release_rows", releaseRows}, kv{"balance_reserved", balReserved}, kv{"pending", pending}), nil
}

// ---- cancel: the user's cancel of a job that has not started ---------------------------------------------------------------------------------------------------------------------------

// cmdCancel cancels a job through the API as its user (a scheduled job: nothing was submitted, the whole reserve comes back) and prints the status the answer carries. It exits 0 only for a 200
// whose job is `cancelled`.
func cmdCancel(args []string) error {
	fs := flags("cancel")
	var api, job string
	fs.StringVar(&api, "api", "http://jobs-a:8080", "the platform instance to call")
	fs.StringVar(&job, "job", "", "the scheduled job")
	if err := parse(fs, args); err != nil {
		return err
	}
	if job == "" {
		return failf(exitUsage, "--job is required")
	}
	ctx, cancel := ctxFor(3 * time.Minute)
	defer cancel()
	sg, err := newSigner()
	if err != nil {
		return err
	}
	d, err := connect(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	h, err := d.head(ctx, job)
	if err != nil {
		return err
	}
	a, err := call(ctx, httpClient(90*time.Second), api, http.MethodPost, "/v1/jobs/"+job+"/cancel", sg.mint(h.User, time.Now()), nil, nil)
	if err != nil {
		return err
	}
	if a.Status != http.StatusOK {
		fact("cancel", "job", job, "outcome", "refused", "status", a.Status, "code", orDash(a.ErrorCode))
		return refused("cancelling the job", a)
	}
	var body jobBody
	if err := json.Unmarshal(a.Body, &body); err != nil || body.Job.Status != "cancelled" {
		return failf(exitFail, "the platform's answer to the cancel is not a cancelled job (status %d, job status %q, request %s)", a.Status, body.Job.Status, orDash(a.RequestID))
	}
	fact("cancel", "job", job, "outcome", "ok", "status", body.Job.Status)
	return nil
}

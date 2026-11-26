package jobengine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The registry: what it makes of a module that is not configured, of a user whose module 1 key cannot be built, of module 1's provisioning and job references, and of the one dial it consumes.

func TestRegistry_AModuleThatIsNotConfiguredWaitsOnEveryCallNamingTheDial(t *testing.T) {
	reg := jobengine.NewRegistry(jobengine.RegistryDeps{}) // nothing configured at all
	for module, dial := range map[string]string{jobs.ModuleVerify: config.VarM1URL, jobs.ModuleSendManaged: config.VarM2URL, jobs.ModuleSendBYO: config.VarM3URL} {
		mod, err := reg.For(ctxT(t), rigUser, module)
		if err != nil || mod == nil {
			t.Fatalf("%s: For = %v, %v; an unconfigured module is a Module whose every call waits, never an error of the job", module, mod, err)
		}
		j := jobOf(module, 2)
		_, e1 := mod.Submit(ctxT(t), j, jobenginetestRows(2))
		_, e2 := mod.Observe(ctxT(t), j, func([]jobs.ResultItem) error { return nil })
		e3 := mod.Cancel(ctxT(t), j)
		for what, err := range map[string]error{"submit": e1, "observe": e2, "cancel": e3} {
			expectWait(t, module+" "+what, err, "not configured", dial)
		}
	}
	// the control: a configured module (the stand-in) answers, so the wait above is the configuration's and not a general one
	_, _, mod := senderModule(t, jobs.ModuleSendManaged, 2)
	if res, err := mod.Submit(ctxT(t), jobOf(jobs.ModuleSendManaged, 2), jobenginetestRows(2)); err != nil || len(res.Accepted) != 2 {
		t.Errorf("the control: %+v, %v", res, err)
	}
	// a module id the jobs table cannot hold is an ordinary error, not a wait
	if _, err := reg.For(ctxT(t), rigUser, "m4"); err == nil || errors.Is(err, jobengine.ErrWait) {
		t.Errorf("For(m4) = %v, want an error that is not ErrWait", err)
	}
	// module 1 with its factory but no tenant resolver cannot derive a key: a wait too
	rig := newM1Rig(t)
	noScope := rig.registry(t, nil, nil)
	mod1, err := noScope.For(ctxT(t), rigUser, jobs.ModuleVerify)
	if err != nil {
		t.Fatalf("For(m1) with no resolver = %v", err)
	}
	_, err = mod1.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 1), jobenginetestRows(1))
	expectWait(t, "module 1 with no tenant resolver", err, "tenant resolver")
}

func TestRegistry_AUserWhoseModuleOneScopeCannotBeBuiltWaitsAndNothingIsSent(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"the key secret is not set", tenant.ErrNoM1KeySecret, config.VarM1KeySecret + " is not set"},
		{"the user was switched off", tenant.ErrInactive, "inactive"},
		{"no such login", tenant.ErrUnknownLogin, "no platform user"},
		{"the database cannot be read", errors.New("connect: refused for user platform_app"), "database"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := newM1Rig(t)
			sc := &rigScoper{key: rig.key, fail: c.err}
			reg := rig.registry(t, sc, nil)
			_, err := reg.For(ctxT(t), rigUser, jobs.ModuleVerify)
			expectWait(t, "For", err, c.want)
			if strings.Contains(err.Error(), "platform_app") || strings.Contains(err.Error(), rig.key) {
				t.Errorf("the reason leaks: %v", err)
			}
			if rig.stub.Count() != 0 {
				t.Errorf("%d request(s) were sent for a user whose scope could not be built", rig.stub.Count())
			}
		})
	}
}

type countingProvisioner struct {
	calls atomic.Int64
	users []string
	fail  error
}

func (p *countingProvisioner) EnsureCaller(_ context.Context, userID string) error {
	p.calls.Add(1)
	p.users = append(p.users, userID)
	return p.fail
}

func TestRegistry_ModuleOnesCallerIsProvisionedOncePerUserAndAFailureWaits(t *testing.T) {
	rig := newM1Rig(t)
	prov := &countingProvisioner{}
	reg := rig.registry(t, &rigScoper{key: rig.key}, prov)
	for i := 0; i < 3; i++ {
		if _, err := reg.For(ctxT(t), rigUser, jobs.ModuleVerify); err != nil {
			t.Fatal(err)
		}
	}
	if prov.calls.Load() != 1 || prov.users[0] != rigUser {
		t.Errorf("the provisioner was called %d time(s) for %v, want once for the user", prov.calls.Load(), prov.users)
	}
	other := "9c1e7a3b-5d2f-4a6c-8b0e-1f3d5a7c9e42"
	if _, err := reg.For(ctxT(t), other, jobs.ModuleVerify); err != nil || prov.calls.Load() != 2 {
		t.Errorf("a second user: %v, %d call(s), want a second provisioning", err, prov.calls.Load())
	}
	// a failing provisioner: the job waits (the caller key does not exist yet, so module 1 would answer 401 to everything) and nothing is sent; it is tried again at the next call
	bad := &countingProvisioner{fail: errors.New("the privileged connection is down")}
	reg2 := rig.registry(t, &rigScoper{key: rig.key}, bad)
	for i := 0; i < 2; i++ {
		_, err := reg2.For(ctxT(t), rigUser, jobs.ModuleVerify)
		expectWait(t, "a provisioning that failed", err, "cannot be provisioned")
	}
	if bad.calls.Load() != 2 || rig.stub.Count() != 0 {
		t.Errorf("a failed provisioning was tried %d time(s) and %d request(s) were sent, want 2 and none", bad.calls.Load(), rig.stub.Count())
	}
	// the control: the nil provisioner means "already provisioned"
	if _, err := rig.registry(t, &rigScoper{key: rig.key}, nil).For(ctxT(t), rigUser, jobs.ModuleVerify); err != nil {
		t.Errorf("no provisioner: %v", err)
	}
}

// A module 1 job's handle is on the job the engine hands over (jobs.EngineJob.ModuleRef, which the store reads with the job): a process that did not submit it (a restart, the new leader) observes it
// from there with no memory and no database of its own, and a job that carries none WAITS, whoever submitted it.
func TestRegistry_AnEngineJobCarryingTheReferenceIsObservedByAnyProcessAndOneWithoutItWaits(t *testing.T) {
	rig := newM1Rig(t)
	scoper := &rigScoper{key: rig.key}
	first := rig.registry(t, scoper, nil)
	mod, _ := first.For(ctxT(t), rigUser, jobs.ModuleVerify)
	rows := jobenginetestRows(3)
	res, err := mod.Submit(ctxT(t), jobOf(jobs.ModuleVerify, 3), rows)
	if err != nil || res.ModuleRef == "" {
		t.Fatalf("Submit = %+v, %v", res, err)
	}
	for _, r := range rows {
		rig.settle(r.ID, charge.Outcome{Module: "m1", State: "valid", Reason: "accepted"})
	}
	observe := func(reg *jobengine.Registry, ref string) (int, error) {
		m, err := reg.For(ctxT(t), rigUser, jobs.ModuleVerify)
		if err != nil {
			t.Fatal(err)
		}
		j := jobOf(jobs.ModuleVerify, 3)
		j.ModuleRef = ref // what the store reads from platform.jobs.module_job_ref
		n := 0
		_, err = m.Observe(ctxT(t), j, func(it []jobs.ResultItem) error { n += len(it); return nil })
		return n, err
	}
	// the control: the process that submitted, handed the job with the reference, and a process that never saw the job, handed the same job: both read the three settled rows
	if n, err := observe(first, res.ModuleRef); err != nil || n != 3 {
		t.Fatalf("the submitting process: %d item(s), %v", n, err)
	}
	fresh := rig.registry(t, scoper, nil)
	if n, err := observe(fresh, res.ModuleRef); err != nil || n != 3 {
		t.Fatalf("a process that did not submit the job, handed its reference: %d item(s), %v; want the three settled rows", n, err)
	}
	// the job without the reference waits, in the fresh process AND in the one that submitted (it remembers nothing: the reference is the job's), asks module 1 nothing and hands over nothing
	before := rig.stub.Count()
	for name, reg := range map[string]*jobengine.Registry{"a process that did not submit the job": fresh, "the process that submitted it": first} {
		n, err := observe(reg, "")
		expectWait(t, name+", the job carrying no reference", err, "m1 observe", "carries no module 1 id")
		if n != 0 {
			t.Errorf("%s handed over %d item(s) for a job that carries no reference", name, n)
		}
	}
	if got := rig.stub.Count(); got != before {
		t.Errorf("%d request(s) were sent to module 1 for a job with no reference: it cannot be read, so nothing is asked", got-before)
	}
	// a reference that is not module 1's id of any job is the module's own refusal, which waits as well (never an invented outcome)
	n, err := observe(fresh, "00000000-0000-4000-8000-0000000000ff")
	expectWait(t, "a reference module 1 does not know", err, "m1 observe")
	if n != 0 {
		t.Errorf("%d item(s) handed over for a reference module 1 does not know", n)
	}
}

func TestRegistry_TheSubmitConcurrencyDialIsHonouredDefaultedAndCapped(t *testing.T) {
	for _, c := range []struct {
		name         string
		dial, rows   int
		wantInFlight int64
	}{
		{"the default when unset", 0, 40, jobengine.DefaultSubmitConcurrency},
		{"the dial", 5, 40, 5},
		{"cut to the ceiling", 1000, 140, jobengine.MaxSubmitConcurrency},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig, _, mod := senderModule(t, jobs.ModuleSendBYO, c.dial)
			rig.delay = 30 * time.Millisecond
			if res, err := mod.Submit(ctxT(t), jobOf(jobs.ModuleSendBYO, c.rows), jobenginetestRows(c.rows)); err != nil || len(res.Accepted) != c.rows {
				t.Fatalf("Submit = %d, %v", len(res.Accepted), err)
			}
			if got := rig.maxIn.Load(); got != c.wantInFlight {
				t.Errorf("%d run calls were in flight together, want %d", got, c.wantInFlight)
			}
		})
	}
	if fmt.Sprint(jobengine.DefaultSubmitConcurrency, jobengine.MaxSubmitConcurrency) != "8 64" {
		t.Errorf("the bounds are %d and %d, the documented 8 and 64 (internal/config's dial has its own range, pinned by its test)", jobengine.DefaultSubmitConcurrency, jobengine.MaxSubmitConcurrency)
	}
}

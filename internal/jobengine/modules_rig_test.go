package jobengine_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/adapters/m2"
	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/charge"
	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobengine"
	"github.com/coldmail-platform/platform/internal/jobengine/jobenginetest"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/modclient"
	"github.com/coldmail-platform/platform/internal/tenant"
)

// The stand-ins of the modules the real wrappers are tested against. They are the test kit of the adapters' own tests (sendapitest.Stub, Envelope, JSON, Campaign: the report with the module's
// keyset paging) with the one thing those do not have, STATE: an item the module holds changes state when the test settles it, so that Submit, Observe and Cancel can be driven in turn.
// Module 1 has no such kit (its adapter's tests keep their stand-in to themselves), so its stand-in is written here from the shapes of the adapter's recorded fixtures. NONE of this is in the
// harness, where the real modules are run and never mocked.

const (
	rigUser     = "2f6d1c1e-7a4b-4c55-9f0e-5b7a3c9d8e21"
	rigCampaign = "5a1c9e30-7d2b-4f6a-9b8e-1c3d5e7f9a02"
)

// rigScoper is the tenant scoper of the tests: it answers the user with a per-run key.
type rigScoper struct {
	key  string
	fail error
	n    atomic.Int64
}

func (s *rigScoper) Scope(_ context.Context, t tenant.Tenant) (tenant.Scope, error) {
	s.n.Add(1)
	if s.fail != nil {
		return tenant.Scope{}, s.fail
	}
	return tenant.Scope{M2UserID: t.UserID, M3UserID: t.UserID, M1Key: config.Secret(s.key)}, nil
}

func jobOf(module string, n int) jobs.EngineJob {
	return jobs.EngineJob{Job: jobs.Job{ID: rigCampaign, UserID: rigUser, Module: module, Status: jobs.StatusConfirmed}, Rows: n, Pending: n}
}

func ctxT(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---- a sender (module 2 or 3) ------------------------------------------------------------------------------------------------------------------------------------------------------

type senderItem struct {
	id, state, reason, errClass string
}

// senderRig is a stateful stand-in of module 2 or 3's platform routes: run (202 for a new job item, 200 for the one it holds), the campaign report (the module's paging, through
// sendapitest.Campaign) and cancel.
type senderRig struct {
	t      *testing.T
	module string
	stub   *sendapitest.Stub
	token  string

	mu       sync.Mutex
	items    map[string]*senderItem
	order    []string
	down     atomic.Bool
	busy     atomic.Int64 // the next n run calls answer 409
	failRun  atomic.Int64 // the next run calls answer this HTTP status (0: none)
	runs     atomic.Int64
	inflight atomic.Int64  // run calls in progress now
	maxIn    atomic.Int64  // the most run calls ever in progress together
	delay    time.Duration // every run call takes this long
	capacity atomic.Int64  // when above 0: the module holds at most this many items and answers 409 to a NEW one beyond it
	events   []string      // "start" and "end" of every run call, in the order they happened
	conflict []string      // the causes module 3 names in its 409 (module 2's 409 has no details)
	// dropListing makes the campaign report's item list omit its last n items while its totals still count them (a listing that contradicts the module's own totals)
	dropListing atomic.Int64
}

func newSenderRig(t *testing.T, module string) *senderRig {
	t.Helper()
	r := &senderRig{t: t, module: module, items: map[string]*senderItem{}, conflict: []string{"campaign_active"}}
	r.stub = sendapitest.NewStub(t, r.handle)
	r.token = sendapitest.Token(t)
	return r
}

// adapters builds the real adapter over the stand-in.
func (r *senderRig) registry(t *testing.T, conc int) *jobengine.Registry {
	t.Helper()
	mod := modclient.Module(r.module)
	mc := sendapitest.ModClient(t, mod, r.stub.URL(), r.token, modclient.Options{})
	d := jobengine.RegistryDeps{SubmitConcurrency: conc}
	switch r.module {
	case jobs.ModuleSendManaged:
		a, err := m2.NewAdapter(mc)
		if err != nil {
			t.Fatal(err)
		}
		d.M2 = a
	default:
		a, err := m3.NewAdapter(mc)
		if err != nil {
			t.Fatal(err)
		}
		d.M3 = a
	}
	return jobengine.NewRegistry(d)
}

func (r *senderRig) itemJSON(it *senderItem) map[string]any {
	completed := any(nil)
	if slices.Contains([]string{"sent", "bounced", "failed", "cancelled"}, it.state) {
		completed = "2026-01-01T00:00:00Z"
	}
	m := map[string]any{"job_item_id": it.id, "state": it.state, "provider_message_id": nil, "completed_at": completed, "attempt_count": 0, "error_class": nil}
	if it.errClass != "" {
		m["error_class"] = it.errClass
	}
	if r.module == jobs.ModuleSendManaged {
		m["reason"] = nil
		if it.reason != "" {
			m["reason"] = it.reason
		}
	} else {
		m["smtp_response"], m["failed_at"], m["account_id"], m["account_username"] = nil, nil, nil, nil
	}
	return m
}

func (r *senderRig) header(total int, items []map[string]any) map[string]any {
	count := func(states ...string) int {
		n := 0
		for _, it := range items {
			if slices.Contains(states, it["state"].(string)) {
				n++
			}
		}
		return n
	}
	terminal := count("sent", "bounced", "failed", "cancelled")
	h := map[string]any{
		"dispatch_paused": false,
		"totals": map[string]any{"total": total, "sent": count("sent"), "bounced": count("bounced"), "failed": count("failed"), "in_flight": count("assigned", "sending"),
			"pending": count("queued"), "waiting_for_capacity": 0, "cancelled": count("cancelled")},
		"progress": map[string]any{"terminal": terminal, "fraction_complete": 0, "is_complete": total > 0 && terminal == total},
	}
	if r.module == jobs.ModuleSendBYO {
		h["totals_by_account"] = []any{}
	}
	return h
}

func (r *senderRig) handle(w http.ResponseWriter, req *http.Request, n int) {
	if r.down.Load() {
		sendapitest.JSON(w, 503, sendapitest.Envelope("unavailable"))
		return
	}
	switch {
	case req.Method == http.MethodPost && req.URL.Path == "/v1/run":
		r.run(w, r.stub.Requests()[n-1].Body)
	case req.Method == http.MethodPost && req.URL.Path == "/v1/cancel":
		r.cancel(w, r.stub.Requests()[n-1].Body)
	case req.Method == http.MethodGet && req.URL.Path == "/v1/report":
		r.mu.Lock()
		items := make([]map[string]any, 0, len(r.order))
		for _, id := range r.order {
			items = append(items, r.itemJSON(r.items[id]))
		}
		r.mu.Unlock()
		listed := items
		if d := int(r.dropListing.Load()); d > 0 && d <= len(items) {
			listed = items[:len(items)-d]
		}
		sendapitest.Campaign{ID: rigCampaign, User: rigUser, Items: listed, Header: func(int) map[string]any { return r.header(len(items), items) }}.Handler()(w, req, 0)
	default:
		sendapitest.JSON(w, 404, sendapitest.Envelope("not_found"))
	}
}

func (r *senderRig) run(w http.ResponseWriter, b []byte) {
	in := r.inflight.Add(1)
	defer r.inflight.Add(-1)
	for {
		m := r.maxIn.Load()
		if in <= m || r.maxIn.CompareAndSwap(m, in) {
			break
		}
	}
	r.runs.Add(1)
	r.mu.Lock()
	r.events = append(r.events, "start")
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.events = append(r.events, "end")
		r.mu.Unlock()
	}()
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	var body struct {
		JobItemID string `json:"job_item_id"`
		UserID    string `json:"user_id"`
	}
	if err := json.Unmarshal(b, &body); err != nil || body.UserID != rigUser {
		sendapitest.JSON(w, 404, sendapitest.Envelope("not_found", [2]string{"job_item_id", "not_found"}))
		return
	}
	if n := r.failRun.Load(); n != 0 {
		code := "validation_failed"
		switch n {
		case 404:
			code = "not_found"
		case 401, 403:
			code = "unauthenticated"
		}
		sendapitest.JSON(w, int(n), sendapitest.Envelope(code, [2]string{"job_item_id", "malformed"}))
		return
	}
	if n := r.busy.Load(); n > 0 && r.busy.Add(-1) >= 0 {
		if r.module == jobs.ModuleSendBYO {
			var details [][2]string
			for _, c := range r.conflict {
				details = append(details, [2]string{c, "conflict"})
			}
			sendapitest.JSON(w, 409, sendapitest.Envelope("conflict", details...))
		} else {
			sendapitest.JSON(w, 409, sendapitest.Envelope("conflict"))
		}
		return
	}
	r.mu.Lock()
	it, held := r.items[body.JobItemID]
	if c := r.capacity.Load(); !held && c > 0 && int64(len(r.order)) >= c {
		r.mu.Unlock()
		if r.module == jobs.ModuleSendBYO {
			sendapitest.JSON(w, 409, sendapitest.Envelope("conflict", [2]string{"queue_cap", "conflict"}))
		} else {
			sendapitest.JSON(w, 409, sendapitest.Envelope("conflict"))
		}
		return
	}
	if !held {
		it = &senderItem{id: body.JobItemID, state: "queued"}
		r.items[it.id] = it
		r.order = append(r.order, it.id)
	}
	state, n := it.state, len(r.order)
	r.mu.Unlock()
	status := 202
	if held {
		status = 200
	}
	sendapitest.JSON(w, status, fmt.Sprintf(`{"job_id":"0d3e5f70-8192-4a3b-8c4d-%012d","state":%q}`, n, state))
}

func (r *senderRig) cancel(w http.ResponseWriter, b []byte) {
	var body struct {
		CampaignID string `json:"campaign_id"`
		UserID     string `json:"user_id"`
	}
	_ = json.Unmarshal(b, &body)
	r.mu.Lock()
	var cancelled, inFlight, done int
	for _, id := range r.order {
		it := r.items[id]
		switch it.state {
		case "queued":
			it.state = "cancelled"
			if r.module == jobs.ModuleSendManaged {
				it.reason = "cancelled"
			}
			cancelled++
		case "cancelled":
			cancelled++
		case "assigned", "sending":
			inFlight++
		default:
			done++
		}
	}
	r.mu.Unlock()
	sendapitest.JSON(w, 200, fmt.Sprintf(`{"campaign_id":%q,"cancelled":%d,"in_flight":%d,"already_terminal":%d}`, body.CampaignID, cancelled, inFlight, done))
}

// settle is the module's worker finishing one item.
func (r *senderRig) settle(ref string, o charge.Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	it := r.items[ref]
	if it == nil {
		r.t.Errorf("the stand-in does not hold the item %s", ref)
		return
	}
	it.state, it.reason = o.State, o.Reason
	if o.State == "bounced" || o.State == "failed" {
		it.errClass = "bounce_permanent"
	}
}

// eventLog is the run calls' start and end events so far.
func (r *senderRig) eventLog() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *senderRig) state(ref string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if it := r.items[ref]; it != nil {
		return it.state
	}
	return ""
}

func (r *senderRig) held() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.order)
}

// ---- module 1 -------------------------------------------------------------------------------------------------------------------------------------------------------------------------

type m1Item struct {
	ref, addr string
	settled   bool
	out       charge.Outcome
}

type m1Job struct {
	id    string
	body  string
	items []*m1Item
}

// m1Rig is a stateful stand-in of module 1's three job routes: the idempotent bulk submit (a replay of the same key and body answers the same job with the replay header, the same key with
// another body is the 409), the job status and the results listing in ordinal order with an opaque cursor.
type m1Rig struct {
	t    *testing.T
	stub *sendapitest.Stub
	key  string

	mu     sync.Mutex
	jobs   map[string]*m1Job
	byKey  map[string]string
	keys   []string // the Idempotency-Key of every bulk submit, in order
	down   atomic.Bool
	paused atomic.Bool // answer the submit with module 1's 503 paused
	// brokenListing makes the results listing drop its last settled row (a module whose listing contradicts its own totals)
	brokenListing atomic.Bool
	// miscount makes the 202 count one row too many (a module whose counts are not the plan's)
	miscount atomic.Bool
	// dupOf marks a settled row as a repeat of the row at this ordinal (module 1's duplicate_of)
	dupOf map[string]int
}

func newM1Rig(t *testing.T) *m1Rig {
	t.Helper()
	r := &m1Rig{t: t, jobs: map[string]*m1Job{}, byKey: map[string]string{}}
	r.stub = sendapitest.NewStub(t, r.handle)
	r.key = sendapitest.Token(t)
	return r
}

func (r *m1Rig) registry(t *testing.T, scoper jobengine.Scoper, prov jobengine.M1Provisioner) *jobengine.Registry {
	t.Helper()
	f, err := m1.NewFactory(r.stub.URL(), modclient.Options{Timeout: 3 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return jobengine.NewRegistry(jobengine.RegistryDeps{M1: f, Scoper: scoper, Provisioner: prov})
}

func (r *m1Rig) handle(w http.ResponseWriter, req *http.Request, n int) {
	if r.down.Load() {
		sendapitest.JSON(w, 503, sendapitest.Envelope("unavailable"))
		return
	}
	path := req.URL.Path
	switch {
	case req.Method == http.MethodPost && path == "/v1/verify/bulk":
		r.submit(w, req, r.stub.Requests()[n-1].Body)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/v1/jobs/") && strings.HasSuffix(path, "/results"):
		r.results(w, req, strings.TrimSuffix(strings.TrimPrefix(path, "/v1/jobs/"), "/results"))
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/v1/jobs/"):
		r.status(w, strings.TrimPrefix(path, "/v1/jobs/"))
	default:
		sendapitest.JSON(w, 404, sendapitest.Envelope("not_found"))
	}
}

func (r *m1Rig) submit(w http.ResponseWriter, req *http.Request, b []byte) {
	if r.paused.Load() {
		sendapitest.JSON(w, 503, sendapitest.Envelope("paused"))
		return
	}
	key := req.Header.Get("Idempotency-Key")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, key)
	if id, ok := r.byKey[key]; ok {
		j := r.jobs[id]
		if j.body != string(b) {
			sendapitest.JSON(w, 409, sendapitest.Envelope("idempotency_key_reused", [2]string{"Idempotency-Key", "conflict"}))
			return
		}
		w.Header().Set("Idempotent-Replayed", "true")
		sendapitest.JSON(w, 202, r.accepted(j))
		return
	}
	var body struct {
		Items []struct {
			ItemID  string `json:"item_id"`
			Address string `json:"address"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &body); err != nil || len(body.Items) == 0 {
		sendapitest.JSON(w, 400, sendapitest.Envelope("validation_failed", [2]string{"items", "required"}))
		return
	}
	j := &m1Job{id: fmt.Sprintf("00000000-0000-4000-8000-%012d", len(r.jobs)+1), body: string(b)}
	for _, it := range body.Items {
		j.items = append(j.items, &m1Item{ref: it.ItemID, addr: strings.ToLower(it.Address)})
	}
	r.jobs[j.id], r.byKey[key] = j, j.id
	sendapitest.JSON(w, 202, r.accepted(j))
}

func (r *m1Rig) accepted(j *m1Job) string {
	seen := map[string]bool{}
	dup := 0
	for _, it := range j.items {
		if seen[it.addr] {
			dup++
		}
		seen[it.addr] = true
	}
	extra := 0
	if r.miscount.Load() {
		extra = 1
	}
	return fmt.Sprintf(`{"job_id":%q,"status":"accepted","counts":{"total":%d,"enqueued":%d,"duplicate":%d,"malformed":0},"processing_paused":false}`, j.id, len(j.items)+extra, len(j.items)-dup+extra, dup)
}

func (r *m1Rig) status(w http.ResponseWriter, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.jobs[id]
	if j == nil {
		sendapitest.JSON(w, 404, sendapitest.Envelope("not_found", [2]string{"id", "not_found"}))
		return
	}
	by := map[string]int{"valid": 0, "invalid": 0, "risky": 0, "unknown": 0}
	settled := 0
	for _, it := range j.items {
		if it.settled {
			settled++
			by[it.out.State]++
		}
	}
	total := len(j.items)
	complete := settled == total
	st := "running"
	if complete {
		st = "complete"
	}
	sendapitest.JSON(w, 200, fmt.Sprintf(`{"job_id":%q,"status":%q,"complete":%t,"counts":{"total":%d,"settled":%d,"pending":%d},"totals":{"submitted":%d,"settled":%d,"pending":%d,"duplicates":0,"malformed":0,"by_status":{"valid":%d,"invalid":%d,"risky":%d,"unknown":%d}},"processing_paused":false}`,
		id, st, complete, total, settled, total-settled, total, settled, total-settled, by["valid"], by["invalid"], by["risky"], by["unknown"]))
}

func (r *m1Rig) results(w http.ResponseWriter, req *http.Request, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.jobs[id]
	if j == nil {
		sendapitest.JSON(w, 404, sendapitest.Envelope("not_found", [2]string{"id", "not_found"}))
		return
	}
	limit := 100
	if n, err := strconv.Atoi(req.URL.Query().Get("limit")); err == nil && n >= 1 && n <= 1000 {
		limit = n
	}
	start := 0
	if c := req.URL.Query().Get("cursor"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(c)
		n, err2 := strconv.Atoi(string(raw))
		if err != nil || err2 != nil || n < 0 {
			sendapitest.JSON(w, 400, sendapitest.Envelope("validation_failed", [2]string{"cursor", "malformed"}))
			return
		}
		start = n
	}
	var rows []map[string]any
	for ord, it := range j.items {
		if !it.settled || ord < start {
			continue
		}
		signals := map[string]any{}
		if it.out.FailureOrigin != "" {
			signals["failure_origin"] = it.out.FailureOrigin
		}
		if it.out.SkipReason != "" {
			signals["smtp_skipped_reason"] = it.out.SkipReason
		}
		row := map[string]any{
			"ordinal": ord, "item_id": it.ref, "status": it.out.State, "reason": it.out.Reason,
			"flags": map[string]any{"from_cache": it.out.FromCache}, "signals": signals,
		}
		if d, ok := r.dupOf[it.ref]; ok {
			row["duplicate_of"] = d
		}
		rows = append(rows, row)
	}
	if r.brokenListing.Load() && len(rows) > 0 {
		rows = rows[:len(rows)-1]
	}
	next := ""
	if len(rows) > limit {
		last := rows[limit-1]["ordinal"].(int)
		rows, next = rows[:limit], base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(last+1)))
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	body, _ := json.Marshal(map[string]any{"job_id": id, "results": rows, "next_cursor": next})
	sendapitest.JSON(w, 200, string(body))
}

func (r *m1Rig) settle(ref string, o charge.Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, j := range r.jobs {
		for _, it := range j.items {
			if it.ref == ref {
				it.settled, it.out = true, o
				return
			}
		}
	}
	r.t.Errorf("the stand-in does not hold the row %s", ref)
}

// settleAll settles every row the stand-in holds with outcomes from finals, in turn.
func (r *m1Rig) settleAll(finals []charge.Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, j := range r.jobs {
		for _, it := range j.items {
			it.settled, it.out = true, finals[n%len(finals)]
			n++
		}
	}
}

// markDuplicate makes the listing say the row repeats the row at this ordinal.
func (r *m1Rig) markDuplicate(ref string, ordinal int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dupOf == nil {
		r.dupOf = map[string]int{}
	}
	r.dupOf[ref] = ordinal
}

func (r *m1Rig) clearDuplicates() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dupOf = nil
}

func (r *m1Rig) jobCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.jobs)
}

func (r *m1Rig) submittedKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.keys)
}

// ---- subjects for the contract -------------------------------------------------------------------------------------------------------------------------------------------------------

// realSubject builds the Subject of a real wrapper over a stand-in for the contract.
func realSubject(t *testing.T, module string, n int) *jobenginetest.Subject {
	t.Helper()
	ctx := ctxT(t)
	s := &jobenginetest.Subject{Job: jobOf(module, n), Rows: jobenginetest.Rows(n), Finals: jobenginetest.FinalsOf(module)}
	var reg *jobengine.Registry
	switch module {
	case jobs.ModuleVerify:
		rig := newM1Rig(t)
		reg = rig.registry(t, &rigScoper{key: sendapitest.Token(t)}, nil)
		s.Settle, s.Down = rig.settle, rig.down.Store
	default:
		rig := newSenderRig(t, module)
		reg = rig.registry(t, 3)
		s.Settle, s.Down = rig.settle, rig.down.Store
	}
	mod, err := reg.For(ctx, rigUser, module)
	if err != nil {
		t.Fatalf("Registry.For(%s): %v", module, err)
	}
	s.Module = mod
	return s
}

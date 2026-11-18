package m1

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

const (
	m1Ready200 = `{"at":"2026-01-01T00:00:00Z","checks":{"db":true,"pepper":true,"queue":true},"config":"env=development smtp=disabled","pepper":"set","ready":true,"status":"ready","supabaseKey":"unset"}`
	m1Ready503 = `{"at":"2026-01-01T00:00:00Z","checks":{"db":true,"pepper":true,"queue":false},"config":"env=development smtp=disabled","pepper":"set","ready":false,"reasons":["queue: TM_QUEUE_ADDR_KEY is not set: the bulk and job routes answer 503 until it is"],"status":"not_ready","supabaseKey":"unset"}`
)

// TestReady_ModuleOnesChecksAndReasonsComeThrough: module 1's readiness document maps to modclient.Readiness: a ready module has its three checks true; a module whose queue is not
// wired is NOT an error but an answer, with the failing check named and module 1's reason kept; the call sends no credential. Control: a bare 503 with no readiness document is an
// error (Unavailable), never a "not ready" answer.
func TestReady_ModuleOnesChecksAndReasonsComeThrough(t *testing.T) {
	var body = m1Ready200
	var status = 200
	f := newFakeM1(t, func(call) (int, string) { return status, body })
	a := f.adapter(t)
	r, err := a.Ready(testCtx(t))
	if err != nil || !r.Ready || !reflect.DeepEqual(r.Checks, map[string]bool{"db": true, "pepper": true, "queue": true}) || len(r.Reasons) != 0 {
		t.Fatalf("ready: %+v %v", r, err)
	}
	if c := f.seen()[0]; c.Path != "/readyz" || c.Auth != "" {
		t.Fatalf("the readiness call was %+v, want GET /readyz with no credential", c)
	}
	body, status = m1Ready503, 503
	r, err = a.Ready(testCtx(t))
	if err != nil || r.Ready || r.HTTPStatus != 503 || !reflect.DeepEqual(r.Failing(), []string{"queue"}) || len(r.Reasons) != 1 {
		t.Fatalf("not ready: %+v %v, want an answer that names the failing check queue and keeps module 1's reason", r, err)
	}
	body, status = ``, 503
	if r, err := a.Ready(testCtx(t)); r != nil || modclient.KindOf(err) != modclient.Unavailable {
		t.Fatalf("control: a bare 503 gave %+v %v, want an Unavailable error", r, err)
	}
}

// TestHealth_IsLivenessWithNoCredential: GET /healthz, answered by a live module with status ok and a time, is a Health; the call carries no credential. Control: a module that
// answers anything else is an error.
func TestHealth_IsLivenessWithNoCredential(t *testing.T) {
	f := serve(t, 200, `{"status":"ok","at":"2026-01-01T00:00:00Z"}`)
	h, err := f.adapter(t).Health(testCtx(t))
	if err != nil || h == nil || h.Status != "ok" {
		t.Fatalf("%+v %v", h, err)
	}
	if c := f.seen()[0]; c.Path != "/healthz" || c.Method != http.MethodGet || c.Auth != "" {
		t.Fatalf("the liveness call was %+v", c)
	}
	g := serve(t, 200, `{"status":"degraded","at":"2026-01-01T00:00:00Z"}`)
	if h, err := g.adapter(t).Health(testCtx(t)); h != nil || modclient.KindOf(err) != modclient.Contract {
		t.Fatalf("control: %+v %v, want a Contract error", h, err)
	}
}

// TestDescribe_IsAskedForAndAnIncompleteAnswerIsRefused replaces the Phase 3 test that pinned "module 1 has no describe, so Describe is NotSupported and sends nothing" (describe_test.go
// holds the module checks): Describe now sends ONE GET /v1/describe with the per-caller key, and a stand-in that would only have answered the old way, with a describe-shaped body that
// lacks what the shape needs, is a Contract error and no value (never NotSupported). Control: the recorded describe is read by the same call.
func TestDescribe_IsAskedForAndAnIncompleteAnswerIsRefused(t *testing.T) {
	f := serve(t, 200, `{"module":"verify","api_version":"v1"}`)
	d, err := f.adapter(t).Describe(testCtx(t))
	if d != nil || modclient.KindOf(err) != modclient.Contract || modclient.KindOf(err) == modclient.NotSupported {
		t.Fatalf("got %+v %v, want a Contract error and no value", d, err)
	}
	if c := f.seen(); len(c) != 1 || c[0].Method != http.MethodGet || c[0].Path != "/v1/describe" || c[0].Auth != "Bearer "+f.key {
		t.Fatalf("the describe call was %+v, want one GET /v1/describe with the key", c)
	}
	g := serve(t, 200, fixture(t, "describe-ok.json"))
	if d, err := g.adapter(t).Describe(testCtx(t)); err != nil || d == nil || d.Module != "verify" {
		t.Fatalf("control: the recorded describe gave %+v %v", d, err)
	}
}

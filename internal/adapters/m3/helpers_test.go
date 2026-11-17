package m3_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m3"
	"github.com/coldmail-platform/platform/internal/adapters/sendapi/sendapitest"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// The tenants and ids of the tests: synthetic, none a secret, none the all-zero system user.
const (
	userA    = "2f6d1c1e-7a4b-4c55-9f0e-5b7a3c9d8e21"
	userB    = "9c1e7a3b-5d2f-4a6c-8b0e-1f3d5a7c9e42"
	campaign = "5a1c9e30-7d2b-4f6a-9b8e-1c3d5e7f9a02"
	jobItem  = "a1b2c3d4-0000-4000-8000-000000000001"
	jobID    = "0d3e5f70-8192-4a3b-8c4d-5e6f708192a3"
	acct1    = "7e8f9a0b-1c2d-4e3f-8a4b-5c6d7e8f9a0b"
	acct2    = "7e8f9a0b-1c2d-4e3f-8a4b-5c6d7e8f9a0c"
	fixtures = "testdata/contract"
)

// handler is a module's answer to one request: the request and its body (the stub of sendapitest has read it already, so a handler gets it here).
type handler = func(w http.ResponseWriter, r *http.Request, body []byte)

func ctxT(t testing.TB) context.Context { return sendapitest.Ctx(t) }

// stack is an adapter for module 3 at a stub: the stub, the tenant-free adapter, the module client and the token.
type stack struct {
	stub  *sendapitest.Stub
	a     *m3.Adapter
	mc    *modclient.Client
	token string
}

// newStub starts a stub whose handler also receives the request body.
func newStub(t testing.TB, h handler) *sendapitest.Stub {
	t.Helper()
	var p atomic.Pointer[sendapitest.Stub]
	s := sendapitest.NewStub(t, func(w http.ResponseWriter, r *http.Request, n int) {
		var body []byte
		if st := p.Load(); st != nil {
			if seen := st.Requests(); n >= 1 && n <= len(seen) {
				body = seen[n-1].Body
			}
		}
		h(w, r, body)
	})
	p.Store(s)
	return s
}

func newStack(t testing.TB, h handler, mopts modclient.Options, opts ...m3.Option) *stack {
	t.Helper()
	if h == nil {
		h = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			sendapitest.JSON(w, 500, sendapitest.Envelope("internal_error"))
		}
	}
	stub := newStub(t, h)
	token := sendapitest.Token(t)
	mc := sendapitest.ModClient(t, modclient.M3, stub.URL(), token, mopts)
	a, err := m3.NewAdapter(mc, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return &stack{stub: stub, a: a, mc: mc, token: token}
}

func (s *stack) client(t testing.TB, user string) *m3.Client {
	t.Helper()
	c, err := s.a.For(user)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func reply(status int, body string) handler {
	return func(w http.ResponseWriter, _ *http.Request, _ []byte) { sendapitest.JSON(w, status, body) }
}

func replyFixture(status int, name string, t testing.TB) handler {
	return reply(status, string(sendapitest.Read(t, fixtures, name)))
}

func wantKind(t testing.TB, err error, want modclient.Kind) *modclient.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want kind %s", want)
	}
	if got := modclient.KindOf(err); got != want {
		t.Fatalf("kind %s, want %s: %v", got, want, err)
	}
	var e *modclient.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v has no *modclient.Error", err)
	}
	return e
}

func isZero(v any) bool { return v == nil || reflect.ValueOf(v).IsZero() }

func anyHas(problems []string, sub string) bool {
	for _, p := range problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

// digest is what a test compares credentials by: a hash, so that no token is ever printed in a failure message.
func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

func str(s string) *string { return &s }

// reflectField reports whether the struct v has a field called name.
func reflectField(v any, name string) (reflect.StructField, bool) {
	return reflect.TypeOf(v).FieldByName(name)
}

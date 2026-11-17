package sendapitest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// Mutation is a deliberate damage to a recorded body that the adapter must refuse: a required key removed, a key's type changed, a value outside a closed set. WantKind is the Kind of the
// refusal (Contract when 0) and WantKey the response key the error must name ("" for a refusal that names none, such as a malformed error envelope, which modclient judges before any key).
type Mutation struct {
	Path    string
	Op      Op
	Value   any
	WantKey string
	// WantKind defaults to modclient.Contract.
	WantKind modclient.Kind
}

// Case is how one contract is exercised: Do makes the call the contract is about against a server at url (building the clients itself) and returns what the adapter returned; Kind is the
// Kind it must fail with (0 for success); Code is the module's error code the error must carry; Check is an extra assertion on the result (it runs for the success of the recorded body and
// for the failure of an error body).
type Case struct {
	Do        func(t testing.TB, url, token string) (any, error)
	Kind      modclient.Kind
	Code      string
	Check     func(t testing.TB, v any, err error)
	Mutations []Mutation
}

// Suite is the contract test of one adapter package: every contract's recorded body is served from an httptest server and decoded through the adapter's real code, a body that does not
// conform to its own Shape is refused, and every mutation is refused with the key it names. Cases is keyed by Key(contract).
type Suite struct {
	Dir       string
	Contracts []modclient.RouteContract
	Cases     map[string]Case
}

// Key is how a Suite names a contract.
func Key(c modclient.RouteContract) string { return c.Route + " | " + c.Case }

// Run runs the suite. A contract with no case, a case with no contract and an empty suite are failures: a contract nothing exercises is no contract.
func (s Suite) Run(t *testing.T) {
	t.Helper()
	if len(s.Contracts) == 0 {
		t.Fatal("the suite has no contracts: the subject is empty")
	}
	named := map[string]bool{}
	for _, c := range s.Contracts {
		k := Key(c)
		named[k] = true
		cs, ok := s.Cases[k]
		if !ok {
			t.Errorf("the contract %q has no case that exercises it", k)
			continue
		}
		t.Run(k, func(t *testing.T) {
			body := Read(t, s.Dir, c.Fixture)
			// the recorded body conforms to the Shape the adapter decodes it with
			if err := modclient.Decode(&modclient.Response{Status: c.Status, Body: body}, nil, c.Shape); err != nil {
				t.Fatalf("the recorded body %s does not conform to its own contract shape: %v", c.Fixture, err)
			}
			t.Run("recorded", func(t *testing.T) {
				v, err := s.do(t, cs, c.Status, body)
				if cs.Kind == 0 {
					if err != nil {
						t.Fatalf("the recorded body is refused: %v", err)
					}
				} else {
					e := wantKind(t, err, cs.Kind)
					if cs.Code != "" && e.Code != cs.Code {
						t.Fatalf("code %q, want %q", e.Code, cs.Code)
					}
					if !zero(v) {
						t.Fatalf("a value came back with the error: %+v", v)
					}
				}
				if cs.Check != nil {
					cs.Check(t, v, err)
				}
			})
			for _, m := range cs.Mutations {
				t.Run("mutated "+m.Path, func(t *testing.T) {
					bad, err := Mutate(body, m.Path, m.Op, m.Value)
					if err != nil {
						t.Fatal(err)
					}
					v, err := s.do(t, cs, c.Status, bad)
					want := m.WantKind
					if want == 0 {
						want = modclient.Contract
					}
					e := wantKind(t, err, want)
					if e.Key != m.WantKey {
						t.Fatalf("the refusal names key %q, want %q: %v", e.Key, m.WantKey, err)
					}
					if !zero(v) {
						t.Fatalf("a value came back with the refusal: %+v", v)
					}
				})
			}
		})
	}
	for k := range s.Cases {
		if !named[k] {
			t.Errorf("the case %q exercises no contract", k)
		}
	}
}

func (s Suite) do(t testing.TB, cs Case, status int, body []byte) (any, error) {
	t.Helper()
	stub := NewStub(t, Serve(status, body))
	return cs.Do(t, stub.URL(), Token(t))
}

// Ctx is a context that ends with the test (or after 20 seconds).
func Ctx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
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

func zero(v any) bool { return v == nil || reflect.ValueOf(v).IsZero() }

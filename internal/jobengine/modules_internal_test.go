package jobengine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// mapError is THE place a module's error becomes the engine's; this table is its contract (the doc of mapError says the same in words).

type errClass int

const (
	classWait errClass = iota + 1
	classRefused
	classNotSupported
)

func classOf(err error) errClass {
	w, r, n := errors.Is(err, ErrWait), errors.Is(err, ErrRefused), errors.Is(err, ErrNotSupported)
	switch {
	case w && !r && !n:
		return classWait
	case r && !w && !n:
		return classRefused
	case n && !w && !r:
		return classNotSupported
	}
	return 0 // none, or more than one: never a valid answer
}

func (c errClass) String() string {
	return map[errClass]string{classWait: "ErrWait", classRefused: "ErrRefused", classNotSupported: "ErrNotSupported"}[c]
}

func TestMapError_KindByPhaseTable(t *testing.T) {
	kinds := []modclient.Kind{modclient.Unavailable, modclient.Timeout, modclient.Paused, modclient.RateLimited, modclient.Canceled, modclient.Contract, modclient.Internal,
		modclient.Refused, modclient.Invalid, modclient.NotFound, modclient.NotSupported}
	phases := []phase{phaseSubmit, phaseObserve, phaseCancel}
	want := func(k modclient.Kind, p phase) errClass {
		switch k {
		case modclient.Refused, modclient.Invalid, modclient.NotFound:
			if p == phaseSubmit {
				return classRefused
			}
		case modclient.NotSupported:
			if p == phaseCancel {
				return classNotSupported
			}
		}
		return classWait
	}
	n := 0
	for _, module := range []string{"m1", "m2", "m3"} {
		for _, k := range kinds {
			for _, p := range phases {
				err := mapError(module, p, &modclient.Error{Kind: k, Module: modclient.Module(module), Status: 400, Code: "validation_failed"})
				n++
				if got := classOf(err); got != want(k, p) {
					t.Errorf("%s %s of kind %s: %s (%v), want %s", module, p, k, got, err, want(k, p))
				}
			}
		}
	}
	if n != 3*11*3 {
		t.Fatalf("the table ran %d cases, want 99: the instrument looked at the wrong thing", n)
	}
	// the control: nil stays nil, and a kind outside the known ones is a wait too (the platform never fails a job for what it does not understand)
	if mapError("m2", phaseSubmit, nil) != nil {
		t.Error("nil became an error")
	}
	if got := classOf(mapError("m2", phaseSubmit, &modclient.Error{Kind: modclient.Kind(99)})); got != classWait {
		t.Errorf("an unknown kind is %s, want ErrWait", got)
	}
}

func TestMapError_TheExceptionsToARefusalAndWhatEachSays(t *testing.T) {
	cases := []struct {
		name     string
		module   string
		p        phase
		err      error
		want     errClass
		contains []string
	}{
		{"a conflict by status (module 2's back-pressure)", "m2", phaseSubmit, &modclient.Error{Kind: modclient.Refused, Status: 409, Code: "conflict"}, classWait, []string{"HTTP 409", "code conflict"}},
		{"a conflict by code alone", "m3", phaseSubmit, &modclient.Error{Kind: modclient.Refused, Code: "conflict"}, classWait, []string{"code conflict"}},
		{"the platform's credential refused: 401", "m2", phaseSubmit, &modclient.Error{Kind: modclient.Refused, Status: 401, Code: "unauthenticated"}, classWait, []string{"credential", "HTTP 401"}},
		{"the platform's credential refused: 403", "m1", phaseSubmit, &modclient.Error{Kind: modclient.Refused, Status: 403, Code: "forbidden"}, classWait, []string{"credential", "HTTP 403"}},
		{"the platform's credential refused: by code", "m3", phaseSubmit, &modclient.Error{Kind: modclient.Refused, Code: "unauthenticated"}, classWait, []string{"credential"}},
		{"a validation refusal", "m2", phaseSubmit, &modclient.Error{Kind: modclient.Refused, Status: 400, Code: "validation_failed"}, classRefused, []string{"validation_failed", "HTTP 400"}},
		{"a not found", "m3", phaseSubmit, &modclient.Error{Kind: modclient.NotFound, Status: 404, Code: "not_found"}, classRefused, []string{"not_found"}},
		{"the idempotency key reused", "m1", phaseSubmit, &modclient.Error{Kind: modclient.Refused, Status: 409, Code: "idempotency_key_reused", Cause: m1.ErrIdempotencyKeyReused}, classRefused, []string{"idempotency key", "platform bug"}},
		{"the idempotency key reused at observe is not a refusal of anything", "m1", phaseObserve, &modclient.Error{Kind: modclient.Refused, Status: 409, Code: "idempotency_key_reused", Cause: m1.ErrIdempotencyKeyReused}, classWait, nil},
		{"a contract violation names its key and says the platform does not understand", "m2", phaseObserve, &modclient.Error{Kind: modclient.Contract, Status: 200, Key: "items.results[0].state"}, classWait, []string{"does not understand", "key items.results[0].state"}},
		{"an internal error of the module says the same", "m3", phaseSubmit, &modclient.Error{Kind: modclient.Internal, Status: 500, Code: "internal_error"}, classWait, []string{"does not understand", "HTTP 500"}},
		{"an error that is already the engine's passes unchanged", "m2", phaseSubmit, Refused("already said"), classRefused, []string{"already said"}},
		{"a wait that wraps a modclient refusal stays a wait", "m2", phaseSubmit, fmt.Errorf("%w: %w", ErrWait, &modclient.Error{Kind: modclient.Refused, Status: 400}), classWait, nil},
		{"a wrapped modclient error is found", "m2", phaseObserve, fmt.Errorf("the walk failed on page 3: %w", &modclient.Error{Kind: modclient.Timeout}), classWait, []string{"timeout"}},
		{"an error of no known kind waits", "m1", phaseObserve, errors.New("something broke"), classWait, []string{"no known kind", "something broke"}},
		{"the context ending waits", "m3", phaseObserve, context.Canceled, classWait, nil},
		{"a cancel the module does not have", "m1", phaseCancel, &modclient.Error{Kind: modclient.NotSupported}, classNotSupported, []string{"no such operation"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := mapError(c.module, c.p, c.err)
			if got := classOf(err); got != c.want {
				t.Fatalf("%s (%v), want %s", got, err, c.want)
			}
			for _, s := range c.contains {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("the text %q does not say %q", err.Error(), s)
				}
			}
		})
	}
}

func TestMapError_NoAddressNoSecretAndNoModuleTextReachesAReason(t *testing.T) {
	const addr = "jane.doe@leak-check.example"
	// built at run time: nothing here is a credential, and nothing that looks like one is written down
	cred := "tok-" + strings.Repeat("0123456789abcdef", 2)
	// the message of a modclient error is a sentence written by the platform, and it is not copied; the text of an error of no known kind is scrubbed
	for name, err := range map[string]error{
		"a modclient error whose message carries an address": &modclient.Error{Kind: modclient.Refused, Status: 400, Code: "validation_failed", Message: "the row for " + addr + " is bad"},
		"an error of no known kind with an address":          errors.New("dial tcp: lookup " + addr + ": no such host"),
		"an error of no known kind with a bearer":            errors.New("Authorization: Bearer " + cred),
		"an error of no known kind with a named secret":      errors.New("GET /x?token=" + cred + "&a=1 failed"),
		"an error of no known kind with URL credentials":     errors.New("Post https://user:" + cred + "@host.example/v1: refused"),
	} {
		for _, p := range []phase{phaseSubmit, phaseObserve, phaseCancel} {
			text := mapError("m2", p, err).Error()
			for _, bad := range []string{addr, "jane.doe", cred, "leak-check"} {
				if strings.Contains(text, bad) {
					t.Errorf("%s at %s: the reason %q carries %q", name, p, text, bad)
				}
			}
		}
	}
}

func TestScrubText_ReplacesWhatMustNotBeLoggedAndKeepsTheWords(t *testing.T) {
	userinfo := "pw" + strings.Repeat("1234567", 2)
	opaque := strings.Repeat("abcd", 4)
	for _, c := range []struct{ in, want, keep string }{
		{"the row for a.b+c@example.org failed", "[address]", "the row for"},
		{"Bearer " + opaque + " was refused", "Bearer [redacted]", "was refused"},
		{"https://user:" + userinfo + "@host.example/x", "https://[redacted]@host.example/x", "host.example"},
		{"GET /v1?api_key=" + opaque + "&page=2", "api_key=[redacted]", "page=2"},
		{"line one\nforged line two\x00", "line one forged line two ", ""},
	} {
		got := scrubText(c.in)
		if !strings.Contains(got, c.want) || (c.keep != "" && !strings.Contains(got, c.keep)) || strings.ContainsAny(got, "\n\x00") || strings.Contains(got, userinfo) || strings.Contains(got, opaque) {
			t.Errorf("scrubText(%q) = %q, want it to hold %q and %q and no control character", c.in, got, c.want, c.keep)
		}
	}
	// the control: plain words are untouched, so a scrubber that blanks everything would be reported
	if got := scrubText("m2 submit: the module cannot answer now (unavailable, HTTP 503, code unavailable)"); got != "m2 submit: the module cannot answer now (unavailable, HTTP 503, code unavailable)" {
		t.Errorf("a clean reason was changed: %q", got)
	}
	// the cut is on a character boundary and says it cut
	long := strings.Repeat("é", 500)
	if got := scrubText(long); len(got) > maxDetailText+3 || !strings.HasSuffix(got, "...") || !strings.HasPrefix(long, strings.TrimSuffix(got, "...")) {
		t.Errorf("a long text was cut to %d bytes: %q...", len(got), got[:20])
	}
}

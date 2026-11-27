package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The unsubscribe link in the store (Phase 5 S3), the parts that need no database. What needs one (the link written into public.job_items, ItemOwner over real rows, the confirm refusal, the column's
// CHECK) is in tests/harness/jobs_unsubscribe_test.go and job_items_unsubscribe_test.go, run by db-tests on the runner. Every refusal here has an accepted twin.

func TestUnsubscribe_BadLinkIsTheColumnsRule(t *testing.T) {
	long := "https://" + strings.Repeat("a", MaxUnsubscribeURLBytes-len("https://"))
	for _, c := range []struct {
		name, link string
		ok         bool
	}{
		{"a link the platform makes", "https://app.example.test/u/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", true},
		{"a path prefix", "https://app.example.test/mail/u/x", true},
		{"a port", "https://app.example.test:8443/u/x", true},
		{"one character after the scheme", "https://a", true},
		{"exactly 2000 bytes", long, true},
		{"a query", "https://app.example.test/u/x?y=1&z=2", true},
		{"empty", "", false},
		{"only the scheme", "https://", false},
		{"http", "http://app.example.test/u/x", false},
		{"upper-case scheme", "HTTPS://app.example.test/u/x", false},
		{"another scheme", "ftp://app.example.test/u/x", false},
		{"no scheme", "app.example.test/u/x", false},
		{"2001 bytes", long + "a", false},
		{"a space", "https://app.example.test/u/a b", false},
		{"a leading space", " https://app.example.test/u/x", false},
		{"a tab", "https://app.example.test/u/a\tb", false},
		{"a newline", "https://app.example.test/u/a\nb", false},
		{"a trailing newline", "https://app.example.test/u/x\n", false},
		{"a carriage return", "https://app.example.test/u/x\r", false},
		{"an opening angle bracket", "https://app.example.test/u/<x", false},
		{"a closing angle bracket", "https://app.example.test/u/x>", false},
		{"a control character", "https://app.example.test/u/\x01", false},
		{"DEL", "https://app.example.test/u/\x7f", false},
		{"a NUL", "https://app.example.test/u/\x00", false},
		{"a no-break space", "https://app.example.test/u/a b", false},
		{"a line separator", "https://app.example.test/u/a b", false},
		{"invalid UTF-8", "https://app.example.test/u/\xff\xfe", false},
	} {
		if reason := badLink(c.link); (reason == "") != c.ok {
			t.Errorf("%s: badLink = %q, accepted=%v", c.name, reason, c.ok)
		} else if reason != "" && strings.Contains(reason, "app.example.test") {
			t.Errorf("%s: the reason quotes the link: %q", c.name, reason)
		}
	}
	if MaxUnsubscribeURLBytes != 2000 {
		t.Fatalf("MaxUnsubscribeURLBytes = %d: platform_0017's CHECK allows 2000 bytes, change them together", MaxUnsubscribeURLBytes)
	}
}

func TestUnsubscribe_LinkOfIsEmptyWithoutAMakerAndOtherwiseTheMakersCheckedLink(t *testing.T) {
	const id = "0f6c1a52-9b7e-4d1c-8a35-2e7b94c0d1aa"
	none := &Store{}
	if none.UnsubscribeConfigured() {
		t.Fatal("a store with no option has a link maker")
	}
	if link, err := none.linkOf(id); err != nil || link != "" {
		t.Fatalf("no maker: (%q, %v), want the empty link and no error (the column is NULL)", link, err)
	}

	var asked []string
	s := &Store{}
	WithUnsubscribe(func(item string) (string, error) {
		asked = append(asked, item)
		return "https://app.example.test/u/" + item, nil
	})(s)
	if !s.UnsubscribeConfigured() {
		t.Fatal("WithUnsubscribe did not configure the store")
	}
	link, err := s.linkOf(id)
	if err != nil || link != "https://app.example.test/u/"+id || len(asked) != 1 || asked[0] != id {
		t.Fatalf("with a maker: (%q, %v), asked %v", link, err, asked)
	}
	// the same item gives the same link: a replay of an insert does not change what an earlier one wrote
	if again, _ := s.linkOf(id); again != link {
		t.Fatalf("a second call gave another link: %q", again)
	}
	// WithUnsubscribe(nil) takes the maker away again (a later option wins)
	WithUnsubscribe(nil)(s)
	if s.UnsubscribeConfigured() {
		t.Fatal("WithUnsubscribe(nil) left a maker")
	}
}

func TestUnsubscribe_AMakerThatFailsOrReturnsABadLinkIsAnErrorThatNeverQuotesTheLink(t *testing.T) {
	const bad = "https://app.example.test/u/has a space-CANARY-5521"
	s := &Store{}
	WithUnsubscribe(func(string) (string, error) { return bad, nil })(s)
	_, err := s.linkOf("0f6c1a52-9b7e-4d1c-8a35-2e7b94c0d1aa")
	if err == nil || strings.Contains(err.Error(), "CANARY") || strings.Contains(err.Error(), "app.example.test") {
		t.Fatalf("a bad link: %v (it must be refused and never quoted)", err)
	}
	s = &Store{}
	WithUnsubscribe(func(string) (string, error) { return "", errors.New("the maker failed with CANARY-5522 in its text") })(s)
	_, err = s.linkOf("0f6c1a52-9b7e-4d1c-8a35-2e7b94c0d1aa")
	if err == nil || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("a failing maker: %v (its text must not be passed on)", err)
	}
	// the control: a good link from the same shape of maker is accepted
	s = &Store{}
	WithUnsubscribe(func(string) (string, error) { return "https://app.example.test/u/x", nil })(s)
	if link, err := s.linkOf("0f6c1a52-9b7e-4d1c-8a35-2e7b94c0d1aa"); err != nil || link == "" {
		t.Fatalf("control failed: (%q, %v)", link, err)
	}
}

func TestUnsubscribe_ItemOwnerRefusesWhatIsNotACanonicalUUIDBeforeAskingTheDatabase(t *testing.T) {
	s := &Store{} // no pool: a call that reached the database would panic, so the test proves the refusal comes first
	for _, id := range []string{"", "x", "0F6C1A52-9B7E-4D1C-8A35-2E7B94C0D1AA", "0f6c1a529b7e4d1c8a352e7b94c0d1aa", "0f6c1a52-9b7e-4d1c-8a35-2e7b94c0d1aa ", "' OR 1=1 --", "0f6c1a52-9b7e-4d1c-8a35-2e7b94c0d1a"} {
		user, email, err := s.ItemOwner(context.Background(), id)
		if !errors.Is(err, ErrNotFound) || user != "" || email != "" {
			t.Errorf("ItemOwner(%q) = (%q, %q, %v), want ErrNotFound and nothing else", id, user, email, err)
		}
		if err != nil && strings.Contains(err.Error(), id) && id != "" {
			t.Errorf("the error quotes the id: %v", err)
		}
	}
}

func TestUnsubscribe_AStoreWithAMakerAsksTheDatabaseNothingBeforeAConfirm(t *testing.T) {
	s := &Store{} // no pool: with a maker the check must not touch it
	WithUnsubscribe(func(string) (string, error) { return "https://app.example.test/u/x", nil })(s)
	if err := s.requireUnsubscribe(context.Background(), "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"); err != nil {
		t.Fatalf("a store with a link maker refused: %v", err)
	}
	// the word the API and the documents use
	if ReasonUnsubscribeNotConfigured != "unsubscribe_not_configured" {
		t.Fatalf("ReasonUnsubscribeNotConfigured = %q: the API maps and Docs/jobs-api.md documents unsubscribe_not_configured", ReasonUnsubscribeNotConfigured)
	}
	var na *NotAllowedError
	if err := notAllowed(ReasonUnsubscribeNotConfigured); !errors.As(err, &na) || !errors.Is(err, ErrNotAllowed) || na.Reason != "unsubscribe_not_configured" {
		t.Fatalf("the refusal is %v, want an ErrNotAllowed carrying the reason", err)
	}
}

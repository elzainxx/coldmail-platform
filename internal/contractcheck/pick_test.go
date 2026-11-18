package contractcheck

import (
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// pickIndex chooses which of several observations of one contract becomes its recorded fixture. Phase 3 close-out D1: it used to take the LONGEST body among equals, which turned a 220-row results
// page of 170 KB into the fixture of module 1's results route. The rule is now: the fewest positions that tell nothing, then the most positions, then the SHORTEST body, then the first.
func TestPickIndex_PrefersWhatSaysMostAndThenWhatCostsLeast(t *testing.T) {
	row := func(extra string) string { return `{"id":"x","state":"sent"` + extra + `}` }
	page := func(rows ...string) []byte { return []byte(`{"items":[` + strings.Join(rows, ",") + `]}`) }
	pick := func(bodies ...[]byte) int {
		return pickIndex(len(bodies), func(i int) []byte { return bodies[i] }, modclient.Shape{})
	}

	one, many := page(row("")), page(row(""), row(""), row(""), row(""), row(""))
	if got := pick(many, one); got != 1 {
		t.Errorf("a page of 5 rows and a page of 1 row with the same skeleton: picked %d, want the shorter (1)", got)
	}
	if got := pick(one, many); got != 0 {
		t.Errorf("the same two in the other order: picked %d, want the shorter (0)", got)
	}
	if got := pick(one, page(row(""))); got != 0 {
		t.Errorf("two identical pages: picked %d, want the first (0)", got)
	}
	// more positions beats shorter: a row that shows an optional key says more than a row that does not, even though the body is longer
	rich := page(row(`,"provider_message_id":"p1"`))
	if got := pick(one, rich); got != 1 {
		t.Errorf("a page that shows an optional key and one that does not: picked %d, want the one that shows it (1)", got)
	}
	// and richer-but-bigger beats richer-but-longer only by length: of two pages that show the key, the shorter wins
	richMany := page(row(`,"provider_message_id":"p1"`), row(""), row(""))
	if got := pick(richMany, rich); got != 1 {
		t.Errorf("two pages that show the optional key: picked %d, want the shorter (1)", got)
	}
	// fewer gaps beats everything: an empty array tells nothing about its elements, so the populated page wins however long it is
	empty := []byte(`{"items":[]}`)
	if got := pick(empty, many); got != 1 {
		t.Errorf("an empty page and a populated page: picked %d, want the populated one (1)", got)
	}
	// a body that cannot be read says nothing and loses to any readable one
	if got := pick([]byte(`not json`), empty); got != 1 {
		t.Errorf("an unreadable body and a readable one: picked %d, want the readable one (1)", got)
	}
}

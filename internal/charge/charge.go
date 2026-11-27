// Package charge decides what one settled outcome costs the customer. It is one pure function over a table.
//
// Decide(Outcome) returns a Decision: Charge (one ledger charge of a named unit at the job's frozen price), Free (final and not charged),
// NotFinal (not terminal, left unsettled) or Unmapped (the module's answer cannot tell an answer from our own failure: flagged, never
// charged, never guessed). The rules are DATA, not code: rules.psv, embedded and parsed ONCE at package initialisation, first matching row
// wins, one row per outcome. The same table, byte for byte, is the fenced block in Docs/phase-2-spec.md between the charge-table markers
// (scripts/check-charge-table.sh and TestRulesFileMatchesSpec fail when they differ), and it is derived from module code, not invented
// (Docs/history/p2-research/m1-verify-outcomes.txt, m2-send-managed-outcomes.txt).
//
// THE UNKNOWN RULE (the owner's, verbatim): "An unknown caused by the recipient's server is charged; one caused by us, or that can't be
// proven either way, is free." Module 1 says who caused an unknown in signals.failure_origin (own, remote, undetermined); the table reads
// it in its `origin` column, so the rule is rows, not code: remote is charged, own and undetermined are free, and an unknown that names no
// origin (an older module) is Unmapped, never charged.
//
// A broken table never runs: the parser refuses a table with the wrong header, a wrong column count, an unknown decision, an origin cell
// that is none of *, own, remote, undetermined, none, a charge row without a unit, a non-charge row with one, a unit that is not one of
// the five seeded units (or belongs to the other module), a duplicate id, a module that is not one of m1, m2, m3 or platform (a wildcard
// module is refused, so an unknown module can never match a row and is always Unmapped), and a row that an earlier row shadows completely
// (it could never match). The panic at initialisation names the row.
//
// Inputs are compared EXACTLY: no trimming, no case folding, and the string "*" in an Outcome is an ordinary value, never a wildcard. An
// origin that is none of own, remote, undetermined or "" therefore matches no origin-specific row; it falls through to the module's
// catch-all row (M1-99), which is Unmapped.
package charge

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"
)

//go:embed rules.psv
var rulesPSV string

// Outcome is what the platform learned about one item from a module (or decided itself), reduced to the columns the table reads.
type Outcome struct {
	// Module is "m1", "m2", "m3" or "platform".
	Module string
	// State is M1's status (valid, invalid, risky, unknown), M2's item state (sent, bounced, failed, queued, assigned, sending, cancelled) or,
	// for Module "platform", one of cancelled, expired, quarantined, infrastructure_failure, pool_exhausted, smtp_login_rejected, duplicate, malformed, missing_field, unrenderable.
	State string
	// Reason is the module's reason: M1's reason, or M2's item reason ("" when the module names none, and for every other module).
	Reason string
	// FromCache is M1's flags.from_cache (never meta.from_cache, which is always false in a bulk result).
	FromCache bool
	// SkipReason is M1's signals.smtp_skipped_reason ("" = absent).
	SkipReason string
	// FailureOrigin is M1's signals.failure_origin: "own", "remote" or "undetermined" ("" = the key is absent: an older module, or a result that is not an unknown).
	// Compared exactly, like every field: any other string matches no origin-specific row.
	FailureOrigin string
}

// Kind is the class of a decision. The zero Kind is none of them, so an uninitialised Decision can never read as a Charge.
type Kind int

const (
	// Charge: one ledger charge row of Decision.Unit at the job's frozen price.
	Charge Kind = iota + 1
	// Free: final and not charged; the reserved credits come back at release.
	Free
	// NotFinal: not terminal; left unsettled.
	NotFinal
	// Unmapped: the module's response cannot tell us whether this is an answer or our own failure. Nothing is charged; the caller flags it.
	Unmapped
)

// String is the word the table uses for the kind.
func (k Kind) String() string {
	switch k {
	case Charge:
		return "charge"
	case Free:
		return "free"
	case NotFinal:
		return "notfinal"
	case Unmapped:
		return "unmapped"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Decision is the answer for one Outcome.
type Decision struct {
	Kind Kind
	// Unit is set only for Charge, and is always one of Units().
	Unit string
	// Rule is the id of the row that decided ("M1-20"), or "none" when no row names the outcome's module and state.
	Rule string
	// Note is the row's note.
	Note string
}

// Rule is one parsed row of the table.
type Rule struct {
	ID     string
	Module string // always a concrete module in a parsed table
	State  string // "*" or an exact value
	Reason string // "*" or an exact value
	Cache  string // "yes", "no" or "*", matched against Outcome.FromCache
	Skip   string // "*", "none" (SkipReason is ""), "capacity" (one of the four capacity tokens) or an exact token
	Origin string // "*", "own", "remote", "undetermined" or "none" (FailureOrigin is ""), matched against Outcome.FailureOrigin
	Kind   Kind
	Unit   string // only for Charge
	Note   string
}

const (
	wildcard = "*"
	// noRule is the Rule of a Decision that no row produced.
	noRule  = "none"
	header  = "id|module|state|reason|cache|skip|origin|decision|unit|note"
	columns = 10
)

// origins are the cell values of the table's origin column other than "*": the three values of M1's signals.failure_origin, and "none" for
// an Outcome whose FailureOrigin is "" (the key absent).
var origins = []string{"own", "remote", "undetermined", "none"}

// units are the five units platform_0002 seeds into platform.unit_costs, each with the module that owns it.
var units = []string{"m1_own", "m1_cache", "m1_vendor", "m2_accepted", "m2_bounced"}

var unitModule = map[string]string{"m1_own": "m1", "m1_cache": "m1", "m1_vendor": "m1", "m2_accepted": "m2", "m2_bounced": "m2"}

var modules = []string{"m1", "m2", "m3", "platform"}

// capacityTokens are the values of M1's signals.smtp_skipped_reason that mean OUR capacity failed (M1-G5): the answer was settled as
// risky/plausible without a probe having been made.
var capacityTokens = []string{"no_eligible_node", "audit_red", "warmup_cap", "rate_limited"}

var kindOfWord = map[string]Kind{"charge": Charge, "free": Free, "notfinal": NotFinal, "unmapped": Unmapped}

var idPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)

// table is the embedded rules, parsed once when the package is initialised. A malformed table panics here, naming the row: it never runs.
var table = mustParse(rulesPSV)

// Decide returns the decision for o: the first row of the table that matches, or Unmapped with Rule "none" when no row names o's module and
// state. It is pure and deterministic, never panics, and never returns Charge for a module or outcome that no row charges.
func Decide(o Outcome) Decision { return decide(table, o) }

// Rules returns a copy of the parsed table, in order (for tests and the gate).
func Rules() []Rule { return append([]Rule(nil), table...) }

// Units returns a copy of the five unit names a Charge decision can carry, in the order platform_0002 seeds them.
func Units() []string { return append([]string(nil), units...) }

func decide(rules []Rule, o Outcome) Decision {
	for _, r := range rules {
		if r.matches(o) {
			return Decision{Kind: r.Kind, Unit: r.Unit, Rule: r.ID, Note: r.Note}
		}
	}
	return Decision{Kind: Unmapped, Rule: noRule, Note: "no row of the charge table names this module and state: flagged, never charged"}
}

func (r Rule) matches(o Outcome) bool {
	return cellMatches(r.Module, o.Module) &&
		cellMatches(r.State, o.State) &&
		cellMatches(r.Reason, o.Reason) &&
		cacheMatches(r.Cache, o.FromCache) &&
		skipMatches(r.Skip, o.SkipReason) &&
		originMatches(r.Origin, o.FailureOrigin)
}

// cellMatches is an exact comparison; only the table's own "*" is a wildcard (the value in an Outcome is never interpreted).
func cellMatches(col, v string) bool { return col == wildcard || col == v }

func cacheMatches(col string, fromCache bool) bool {
	switch col {
	case wildcard:
		return true
	case "yes":
		return fromCache
	case "no":
		return !fromCache
	}
	return false
}

func skipMatches(col, v string) bool {
	switch col {
	case wildcard:
		return true
	case "none":
		return v == ""
	case "capacity":
		return isCapacity(v)
	}
	return col == v
}

// originMatches: "none" means the Outcome names no origin (""), and nothing else; own, remote and undetermined are exact. A garbage value
// (a string that is none of those, "none" and "*" included) is matched by no cell but the table's own "*".
func originMatches(col, v string) bool {
	switch col {
	case wildcard:
		return true
	case "none":
		return v == ""
	}
	return col == v
}

func isCapacity(v string) bool {
	for _, t := range capacityTokens {
		if v == t {
			return true
		}
	}
	return false
}

// mustParse parses a table or panics with a message that names the offending row.
func mustParse(src string) []Rule {
	rules, err := parse(src)
	if err != nil {
		panic("charge: rules.psv is malformed and must never run: " + err.Error())
	}
	return rules
}

// parse reads a table and refuses a malformed one. Every error names the row (its id and line), except an error about the table as a whole.
func parse(src string) ([]Rule, error) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("the table is empty: a table with no rows decides nothing")
	}
	lines := strings.Split(strings.TrimSuffix(src, "\n"), "\n")
	if lines[0] != header {
		return nil, fmt.Errorf("line 1: the header must be exactly %q, got %q", header, lines[0])
	}
	if len(lines) == 1 {
		return nil, fmt.Errorf("the table has a header and no rows")
	}
	var rules []Rule
	firstLine := map[string]int{}
	for i, line := range lines[1:] {
		n := i + 2
		r, err := parseRow(line, n)
		if err != nil {
			return nil, err
		}
		if first, dup := firstLine[r.ID]; dup {
			return nil, rowErr(r.ID, n, "duplicate id (first used on line %d)", first)
		}
		firstLine[r.ID] = n
		rules = append(rules, r)
	}
	for j, r := range rules {
		for _, e := range rules[:j] {
			if covers(e, r) {
				return nil, rowErr(r.ID, j+2, "shadowed by the earlier row %s: every outcome it names is decided there first, so it can never match", e.ID)
			}
		}
	}
	return rules, nil
}

func rowErr(id string, line int, format string, args ...any) error {
	return fmt.Errorf("row %s (line %d): %s", id, line, fmt.Sprintf(format, args...))
}

func parseRow(line string, n int) (Rule, error) {
	f := strings.Split(line, "|")
	id := f[0]
	if id == "" {
		id = "?"
	}
	if len(f) != columns {
		return Rule{}, rowErr(id, n, "has %d columns, want %d (%s)", len(f), columns, header)
	}
	r := Rule{ID: f[0], Module: f[1], State: f[2], Reason: f[3], Cache: f[4], Skip: f[5], Origin: f[6], Unit: f[8], Note: f[9]}
	if !idPattern.MatchString(r.ID) {
		return Rule{}, rowErr(id, n, "the id must look like M1-17 or P-01")
	}
	if !contains(modules, r.Module) {
		return Rule{}, rowErr(id, n, "module %q is not one of %s (a wildcard module is refused: a row must name the module it rules on, so an unknown module can never match)", r.Module, strings.Join(modules, ", "))
	}
	for _, c := range []struct{ name, v string }{{"state", r.State}, {"reason", r.Reason}, {"skip", r.Skip}} {
		if !validCell(c.v) {
			return Rule{}, rowErr(id, n, "the %s cell %q is empty, has whitespace, or is a partial wildcard (use *, or an exact value)", c.name, c.v)
		}
	}
	switch r.Cache {
	case "yes", "no", wildcard:
	default:
		return Rule{}, rowErr(id, n, "the cache cell %q must be yes, no or *", r.Cache)
	}
	if r.Origin != wildcard && !contains(origins, r.Origin) {
		return Rule{}, rowErr(id, n, "the origin cell %q must be *, %s", r.Origin, strings.Join(origins, ", "))
	}
	k, ok := kindOfWord[f[7]]
	if !ok {
		return Rule{}, rowErr(id, n, "unknown decision %q (want charge, free, notfinal or unmapped)", f[7])
	}
	r.Kind = k
	if k == Charge {
		if r.Unit == "" {
			return Rule{}, rowErr(id, n, "a charge row needs a unit (one of %s)", strings.Join(units, ", "))
		}
		owner, known := unitModule[r.Unit]
		if !known {
			return Rule{}, rowErr(id, n, "unit %q is not one of the seeded units (%s)", r.Unit, strings.Join(units, ", "))
		}
		if owner != r.Module {
			return Rule{}, rowErr(id, n, "unit %q belongs to module %s, not %s", r.Unit, owner, r.Module)
		}
	} else if r.Unit != "" {
		return Rule{}, rowErr(id, n, "decision %q must not carry a unit (found %q): only a charge row names one", f[7], r.Unit)
	}
	return r, nil
}

// validCell: a state, reason or skip cell is "*" or an exact value, with no whitespace and no partial wildcard.
func validCell(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	return s == wildcard || !strings.Contains(s, wildcard)
}

// covers reports whether e decides every outcome that r could name, so that r can never be reached (a conservative test: one earlier row at a time).
func covers(e, r Rule) bool {
	return (e.Module == wildcard || e.Module == r.Module) &&
		(e.State == wildcard || e.State == r.State) &&
		(e.Reason == wildcard || e.Reason == r.Reason) &&
		(e.Cache == wildcard || e.Cache == r.Cache) &&
		(e.Skip == wildcard || e.Skip == r.Skip || (e.Skip == "capacity" && isCapacity(r.Skip))) &&
		(e.Origin == wildcard || e.Origin == r.Origin)
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

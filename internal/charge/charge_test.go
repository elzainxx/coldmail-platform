package charge

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---- the vocabulary and the research facts the tests are written against ------------------------------------------------------------------------------------------------------------------

// legalStatuses and legalReasons are module 1's frozen vocabulary (coldmail-verify control-plane/internal/vocab/vocab.go at the pinned commit): every value a
// verdict may legally carry, whether or not the code ever emits it.
var legalStatuses = []string{"valid", "invalid", "risky", "unknown"}

var legalReasons = []string{
	"invalid_syntax", "invalid_domain", "no_mx", "null_mx", "dns_error", "disposable", "blacklisted_domain", "blacklisted_mx", "spam_trap", "role_account", "whitelisted",
	"provider_no_smtp", "plausible", "unknown_error", "accepted", "mailbox_not_found", "mailbox_full", "catch_all", "accept_all", "greylisted", "smtp_blocked", "smtp_timeout",
	"smtp_error", "connection_failed", "external_verified", "historical_bounce", "non_public_mx",
}

// emittedM1Pairs are the status/reason pairs the pinned module's non-test code can emit (Docs/history/p2-research/m1-verify-outcomes.txt, "REASONS EMITTED"), apart from
// external_verified (any status: the vendor path never runs in the shipped binary, M1-G3).
var emittedM1Pairs = []string{
	"invalid/invalid_syntax", "invalid/no_mx", "invalid/null_mx", "invalid/spam_trap", "risky/disposable", "risky/provider_no_smtp", "risky/plausible", "unknown/dns_error",
	"valid/accepted", "valid/mailbox_full", "invalid/mailbox_not_found", "risky/catch_all", "risky/non_public_mx", "unknown/greylisted", "unknown/smtp_blocked",
	"unknown/smtp_timeout", "unknown/smtp_error", "unknown/connection_failed", "invalid/historical_bounce",
	"unknown/unknown_error", // since A5: the attempt cap settles a decrypt/compute error as unknown/unknown_error with failure_origin own
}

// neverEmittedM1Reasons are legal in the vocabulary and emitted by no non-test code ("LEGAL BUT NEVER EMITTED"). unknown_error left this list with A5 (the attempt cap).
var neverEmittedM1Reasons = []string{"invalid_domain", "blacklisted_domain", "blacklisted_mx", "role_account", "whitelisted", "accept_all"}

// legalM2States are the item states module 2 defines (cancelled since B4).
var legalM2States = []string{"sent", "bounced", "failed", "queued", "assigned", "sending", "cancelled"}

// originValues are the values of M1's signals.failure_origin plus the absent key; garbageOrigins are strings the table must never read as an origin.
var originValues = []string{"", "own", "remote", "undetermined"}

var garbageOrigins = []string{"bogus", "Remote", "OWN", "remote ", " own", "none", "*", "undetermined\n"}

// ---- one case per row, the input written out by hand -------------------------------------------------------------------------------------------------------------------------------------

func chg(rule, unit string) Decision { return Decision{Kind: Charge, Unit: unit, Rule: rule} }
func fre(rule string) Decision       { return Decision{Kind: Free, Rule: rule} }
func nfi(rule string) Decision       { return Decision{Kind: NotFinal, Rule: rule} }
func unm(rule string) Decision       { return Decision{Kind: Unmapped, Rule: rule} }

func m1(state, reason string) Outcome { return Outcome{Module: "m1", State: state, Reason: reason} }

// m1o is an M1 outcome that names a failure origin (M1's signals.failure_origin).
func m1o(state, reason, origin string) Outcome {
	return Outcome{Module: "m1", State: state, Reason: reason, FailureOrigin: origin}
}

func m2(state, reason string) Outcome { return Outcome{Module: "m2", State: state, Reason: reason} }

func describe(d Decision) string { return fmt.Sprintf("%s unit=%q rule=%s", d.Kind, d.Unit, d.Rule) }

// sameDecision compares what a decision commits to (kind, unit, row); the note is prose.
func sameDecision(a, b Decision) bool {
	return a.Kind == b.Kind && a.Unit == b.Unit && a.Rule == b.Rule
}

type rowCase struct {
	id   string
	in   Outcome
	want Decision
}

var rowCases = []rowCase{
	{"M1-01", Outcome{Module: "m1", State: "valid", Reason: "accepted", FromCache: true}, chg("M1-01", "m1_cache")},
	{"M1-02", Outcome{Module: "m1", State: "risky", Reason: "external_verified"}, chg("M1-02", "m1_vendor")},
	{"M1-03", m1("valid", "accepted"), chg("M1-03", "m1_own")},
	{"M1-04", m1("valid", "mailbox_full"), chg("M1-04", "m1_own")},
	{"M1-05", m1("invalid", "no_mx"), chg("M1-05", "m1_own")},
	{"M1-06", m1("invalid", "null_mx"), chg("M1-06", "m1_own")},
	{"M1-07", m1("invalid", "spam_trap"), chg("M1-07", "m1_own")},
	{"M1-08", m1("invalid", "mailbox_not_found"), chg("M1-08", "m1_own")},
	{"M1-09", m1("invalid", "historical_bounce"), chg("M1-09", "m1_own")},
	{"M1-10", m1("invalid", "invalid_syntax"), chg("M1-10", "m1_own")},
	{"M1-11", m1("risky", "disposable"), chg("M1-11", "m1_own")},
	{"M1-12", Outcome{Module: "m1", State: "risky", Reason: "provider_no_smtp", SkipReason: "provider_mx_routed"}, chg("M1-12", "m1_own")},
	{"M1-13", m1("risky", "catch_all"), chg("M1-13", "m1_own")},
	{"M1-14", m1("risky", "non_public_mx"), chg("M1-14", "m1_own")},
	{"M1-15", Outcome{Module: "m1", State: "risky", Reason: "plausible", SkipReason: "no_eligible_node"}, fre("M1-15")},
	{"M1-16", m1("risky", "plausible"), chg("M1-16", "m1_own")},
	{"M1-17", Outcome{Module: "m1", State: "risky", Reason: "plausible", SkipReason: "smtp_unwired"}, fre("M1-17")},
	{"M1-18", m1o("unknown", "greylisted", "remote"), chg("M1-18", "m1_own")},
	{"M1-19", m1o("unknown", "unknown_error", "own"), fre("M1-19")},
	{"M1-20", m1o("unknown", "smtp_timeout", "remote"), chg("M1-20", "m1_own")},
	{"M1-21", m1o("unknown", "smtp_timeout", "own"), fre("M1-21")},
	{"M1-22", m1o("unknown", "smtp_timeout", "undetermined"), fre("M1-22")},
	{"M1-23", m1("unknown", "smtp_timeout"), unm("M1-23")},
	{"M1-99", m1("invalid", "role_account"), unm("M1-99")},
	{"M2-01", Outcome{Module: "m2", State: "sent", FromCache: true, SkipReason: "audit_red", FailureOrigin: "remote"}, chg("M2-01", "m2_accepted")},
	{"M2-02", Outcome{Module: "m2", State: "bounced"}, chg("M2-02", "m2_bounced")},
	{"M2-03", Outcome{Module: "m2", State: "failed", Reason: "attempts_exhausted", FromCache: true}, fre("M2-03")},
	{"M2-04", Outcome{Module: "m2", State: "failed", Reason: "pool_exhausted"}, fre("M2-04")},
	{"M2-05", Outcome{Module: "m2", State: "queued"}, nfi("M2-05")},
	{"M2-06", Outcome{Module: "m2", State: "assigned"}, nfi("M2-06")},
	{"M2-07", Outcome{Module: "m2", State: "sending"}, nfi("M2-07")},
	{"M2-08", Outcome{Module: "m2", State: "cancelled"}, fre("M2-08")},
	{"M2-99", Outcome{Module: "m2", State: "refunded"}, unm("M2-99")},
	{"M3-01", Outcome{Module: "m3", State: "sent"}, fre("M3-01")},
	{"P-01", Outcome{Module: "platform", State: "cancelled"}, fre("P-01")},
	{"P-02", Outcome{Module: "platform", State: "expired"}, fre("P-02")},
	{"P-03", Outcome{Module: "platform", State: "quarantined"}, fre("P-03")},
	{"P-04", Outcome{Module: "platform", State: "infrastructure_failure"}, fre("P-04")},
	{"P-05", Outcome{Module: "platform", State: "pool_exhausted"}, fre("P-05")},
	{"P-06", Outcome{Module: "platform", State: "smtp_login_rejected"}, fre("P-06")},
	{"P-07", Outcome{Module: "platform", State: "duplicate"}, fre("P-07")},
}

// TestDecide_Rows drives one subtest per row of the embedded table; each row's input and expected decision are written out in rowCases, so a table edit that changes what a
// row decides fails here, in the subtest named by the row id.
func TestDecide_Rows(t *testing.T) {
	byID := map[string]rowCase{}
	for _, c := range rowCases {
		byID[c.id] = c
	}
	rules := Rules()
	if len(rules) < 30 {
		t.Fatalf("the embedded table has %d rows: the instrument matched too little (floor 30)", len(rules))
	}
	for _, r := range rules {
		c, ok := byID[r.ID]
		t.Run(r.ID, func(t *testing.T) {
			if !ok {
				t.Fatalf("row %s has no explicit case in rowCases", r.ID)
			}
			got := Decide(c.in)
			if !sameDecision(got, c.want) {
				t.Errorf("row %s: Decide(%+v) = {%s}, want {%s}", r.ID, c.in, describe(got), describe(c.want))
			}
		})
	}
}

// TestDecide_EveryRowHasACase proves the coverage in both directions: a row of the table without a case, a case naming a row that does not exist, and a row with two cases.
func TestDecide_EveryRowHasACase(t *testing.T) {
	count := map[string]int{}
	for _, c := range rowCases {
		count[c.id]++
	}
	rows := map[string]bool{}
	for _, r := range Rules() {
		rows[r.ID] = true
		if count[r.ID] == 0 {
			t.Errorf("row %s has no case in rowCases", r.ID)
		}
	}
	for id, n := range count {
		if !rows[id] {
			t.Errorf("rowCases names row %s, which is not in the embedded table", id)
		}
		if n > 1 {
			t.Errorf("rowCases has %d cases for row %s (one per row)", n, id)
		}
	}
	if len(rows) == 0 || len(count) == 0 {
		t.Fatalf("empty subject: %d table rows, %d cases", len(rows), len(count))
	}
}

// ---- precedence ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestDecide_Precedence(t *testing.T) {
	expect := func(o Outcome, want Decision) {
		t.Helper()
		if got := Decide(o); !sameDecision(got, want) {
			t.Errorf("Decide(%+v) = {%s}, want {%s}", o, describe(got), describe(want))
		}
	}
	// from_cache beats every status, every reason (the vendor answer and the failures included) and every skip token: the answer is the value.
	// The failure origin does not change that either: a cached answer is charged as a cache answer whatever origin it carries (M1 never caches an unknown, so this is the table's own order).
	for _, st := range legalStatuses {
		for _, re := range legalReasons {
			for _, skip := range []string{"", "audit_red", "provider_mx_routed", "smtp_unwired"} {
				for _, origin := range append([]string{"bogus"}, originValues...) {
					expect(Outcome{Module: "m1", State: st, Reason: re, FromCache: true, SkipReason: skip, FailureOrigin: origin}, chg("M1-01", "m1_cache"))
				}
			}
		}
	}
	expect(Outcome{Module: "m1", State: "risky", Reason: "external_verified", FromCache: true}, chg("M1-01", "m1_cache")) // a cached vendor answer is cache, not vendor
	expect(Outcome{Module: "m1", State: "invalid", Reason: "no_mx", FromCache: true}, chg("M1-01", "m1_cache"))           // a cached invalid is cache
	// external_verified, uncached, is the vendor unit whatever the status (and a capacity token does not change that).
	for _, st := range legalStatuses {
		for _, skip := range append([]string{""}, capacityTokens...) {
			expect(Outcome{Module: "m1", State: st, Reason: "external_verified", SkipReason: skip}, chg("M1-02", "m1_vendor"))
		}
	}
	// plausible: each of the four capacity tokens is OUR failure and is free (M1-15); smtp_unwired is our configuration and is free (M1-17); no token is a cheap-layers answer and is charged (M1-16).
	for _, tok := range capacityTokens {
		expect(Outcome{Module: "m1", State: "risky", Reason: "plausible", SkipReason: tok}, fre("M1-15"))
	}
	expect(Outcome{Module: "m1", State: "risky", Reason: "plausible", SkipReason: "smtp_unwired"}, fre("M1-17"))
	expect(Outcome{Module: "m1", State: "risky", Reason: "plausible"}, chg("M1-16", "m1_own"))
	// plausible with a token the table does not know is neither: Unmapped, by the catch-all.
	for _, tok := range []string{"provider_mx_routed", "none", "capacity", "Audit_Red", " audit_red", "audit_red ", "*", "Smtp_Unwired", "smtp_unwired ", " smtp_unwired", "smtp-unwired"} {
		expect(Outcome{Module: "m1", State: "risky", Reason: "plausible", SkipReason: tok}, unm("M1-99"))
	}
	// the token never overrides the cache (a capacity-skipped answer is never cached by the module, so this is the table's own order, not a reachable case).
	expect(Outcome{Module: "m1", State: "risky", Reason: "plausible", FromCache: true, SkipReason: "warmup_cap"}, chg("M1-01", "m1_cache"))
}

// ---- the unknown rule: who caused it ------------------------------------------------------------------------------------------------------------------------------------------------------------------

// THE OWNER'S RULE: "An unknown caused by the recipient's server is charged; one caused by us, or that can't be proven either way, is free." Module 1 says who caused an unknown in
// signals.failure_origin; the tests below pin what the table does with each origin, with the rule id that decided, and each has its paired control (the same outcome with another origin).

// wantUnknownDecision is the table's intended decision for an uncached unknown of any reason except external_verified, written out once, from the rule and not from rules.psv.
func wantUnknownDecision(reason, origin string) Decision {
	switch origin {
	case "remote":
		if reason == "greylisted" {
			return chg("M1-18", "m1_own") // retries exhausted: the recipient's server kept deferring
		}
		return chg("M1-20", "m1_own")
	case "own":
		if reason == "unknown_error" {
			return fre("M1-19") // the attempt cap
		}
		return fre("M1-21")
	case "undetermined":
		return fre("M1-22")
	case "":
		return unm("M1-23") // an older module that names no origin: never charged, reported
	}
	return unm("M1-99") // a string the module never emits
}

func TestDecide_UnknownByOrigin(t *testing.T) {
	emittedReasons := []string{"greylisted", "dns_error", "smtp_timeout", "smtp_error", "connection_failed", "smtp_blocked", "unknown_error"}
	var reasons []string
	reasons = append(reasons, emittedReasons...)
	for _, re := range legalReasons { // every other legal reason too: the origin decides, not the reason (external_verified is the vendor row's: its own test)
		if re != "external_verified" && !contains(reasons, re) {
			reasons = append(reasons, re)
		}
	}
	allOrigins := append(append([]string{}, originValues...), garbageOrigins...)
	charged, free, unmapped, examined := 0, 0, 0, 0
	for _, re := range reasons {
		for _, og := range allOrigins {
			o := m1o("unknown", re, og)
			got, want := Decide(o), wantUnknownDecision(re, og)
			examined++
			if !sameDecision(got, want) {
				t.Errorf("unknown/%s with origin %q: Decide = {%s}, want {%s}", re, og, describe(got), describe(want))
			}
			switch got.Kind {
			case Charge:
				charged++
				if og != "remote" {
					t.Errorf("unknown/%s with origin %q was CHARGED ({%s}): only the recipient's server's unknown is charged", re, og, describe(got))
				}
			case Free:
				free++
			case Unmapped:
				unmapped++
			}
		}
	}
	// the paired control: the sweep above must have seen every kind, and exactly the remote origin charged (one per reason): a table that frees or charges everything fails here.
	if charged != len(reasons) || free == 0 || unmapped == 0 || examined != len(reasons)*len(allOrigins) {
		t.Errorf("examined %d outcomes: charged=%d (want %d, one per reason, all remote) free=%d unmapped=%d", examined, charged, len(reasons), free, unmapped)
	}
	// the headline rows, spelled out with the origin as the only difference
	for _, re := range emittedReasons {
		remote, own, und, none := Decide(m1o("unknown", re, "remote")), Decide(m1o("unknown", re, "own")), Decide(m1o("unknown", re, "undetermined")), Decide(m1("unknown", re))
		if remote.Kind != Charge || remote.Unit != "m1_own" {
			t.Errorf("unknown/%s, remote: {%s}, want a charge of m1_own", re, describe(remote))
		}
		if own.Kind != Free || und.Kind != Free {
			t.Errorf("unknown/%s: own {%s} and undetermined {%s} must both be free", re, describe(own), describe(und))
		}
		if none.Kind != Unmapped {
			t.Errorf("unknown/%s with no origin: {%s}, want unmapped", re, describe(none))
		}
	}
	// a cached unknown is a cache answer whatever its origin (M1-01 is above every origin row), and the same outcome uncached is not
	for _, og := range allOrigins {
		if got := Decide(Outcome{Module: "m1", State: "unknown", Reason: "greylisted", FromCache: true, FailureOrigin: og}); !sameDecision(got, chg("M1-01", "m1_cache")) {
			t.Errorf("a cached unknown with origin %q: {%s}, want M1-01 m1_cache", og, describe(got))
		}
	}
}

// TestDecide_OriginIsReadOnlyForUnknowns: the origin column changes nothing for any outcome that is not an uncached unknown: for every module, legal status and reason, with and without a cache
// flag or a skip token, an origin (any value, a garbage one included) leaves the decision exactly as it is with no origin. The control: for an uncached unknown it DOES change.
func TestDecide_OriginIsReadOnlyForUnknowns(t *testing.T) {
	allOrigins := append(append([]string{}, originValues[1:]...), garbageOrigins...)
	examined, changedForUnknown := 0, 0
	for _, module := range modules {
		for _, st := range append(append([]string{}, legalStatuses...), legalM2States...) {
			for _, re := range append([]string{""}, legalReasons...) {
				for _, cached := range []bool{false, true} {
					for _, skip := range []string{"", "audit_red", "smtp_unwired"} {
						base := Outcome{Module: module, State: st, Reason: re, FromCache: cached, SkipReason: skip}
						baseDecision := Decide(base)
						for _, og := range allOrigins {
							with := base
							with.FailureOrigin = og
							examined++
							got := Decide(with)
							ruledByOrigin := module == "m1" && st == "unknown" && !cached && re != "external_verified"
							if ruledByOrigin {
								if !sameDecision(got, baseDecision) {
									changedForUnknown++
								}
								continue
							}
							if !sameDecision(got, baseDecision) {
								t.Fatalf("%+v: the origin %q changed the decision {%s} -> {%s}: only an uncached M1 unknown reads the origin", base, og, describe(baseDecision), describe(got))
							}
						}
					}
				}
			}
		}
	}
	if examined < 10000 {
		t.Fatalf("examined %d outcomes: the instrument matched too little", examined)
	}
	if changedForUnknown == 0 {
		t.Fatal("no uncached unknown changed with its origin: the control shows the origin is not read at all")
	}
}

// TestDecide_AttemptCap: M1's attempt cap (A5) settles a decrypt or compute error that repeated to the cap as unknown/unknown_error with origin own: free (M1-19). Controls: the same reason with
// another origin is not M1-19, and another reason with origin own is the generic own row (M1-21).
func TestDecide_AttemptCap(t *testing.T) {
	cases := []struct {
		name string
		in   Outcome
		want Decision
	}{
		{"the attempt cap: unknown_error, own", m1o("unknown", "unknown_error", "own"), fre("M1-19")},
		{"control: unknown_error, remote is not the cap", m1o("unknown", "unknown_error", "remote"), chg("M1-20", "m1_own")},
		{"control: unknown_error, undetermined", m1o("unknown", "unknown_error", "undetermined"), fre("M1-22")},
		{"control: unknown_error with no origin", m1("unknown", "unknown_error"), unm("M1-23")},
		{"control: unknown_error with a garbage origin", m1o("unknown", "unknown_error", "Own"), unm("M1-99")},
		{"control: another reason, own, is the generic row", m1o("unknown", "smtp_error", "own"), fre("M1-21")},
		{"control: greylisted, own, is the generic row (never charged)", m1o("unknown", "greylisted", "own"), fre("M1-21")},
		{"control: valid/unknown_error is not an unknown", m1o("valid", "unknown_error", "own"), unm("M1-99")},
	}
	for _, c := range cases {
		if got := Decide(c.in); !sameDecision(got, c.want) {
			t.Errorf("%s: Decide(%+v) = {%s}, want {%s}", c.name, c.in, describe(got), describe(c.want))
		}
	}
}

// TestDecide_SmtpUnwired: SMTP was not wired at boot (A4): risky/plausible with the token smtp_unwired is OUR configuration and free (M1-17); the same outcome with no token is a cheap-layers
// answer and is charged (M1-16); with a capacity token it is M1-15. The token changes nothing for the rows that do not rule on plausible.
func TestDecide_SmtpUnwired(t *testing.T) {
	plausible := func(tok string) Outcome {
		return Outcome{Module: "m1", State: "risky", Reason: "plausible", SkipReason: tok}
	}
	for _, c := range []struct {
		tok  string
		want Decision
	}{
		{"smtp_unwired", fre("M1-17")},
		{"", chg("M1-16", "m1_own")}, // the control: with the wired SMTP stage skipped by nothing, the answer is the cheap layers'
		{"no_eligible_node", fre("M1-15")}, {"audit_red", fre("M1-15")}, {"warmup_cap", fre("M1-15")}, {"rate_limited", fre("M1-15")},
	} {
		if got := Decide(plausible(c.tok)); !sameDecision(got, c.want) {
			t.Errorf("risky/plausible with the token %q: {%s}, want {%s}", c.tok, describe(got), describe(c.want))
		}
	}
	// the token is read only by the plausible rows: a DNS fact or a remote unknown keeps its own row
	for _, c := range []struct {
		in   Outcome
		want Decision
	}{
		{Outcome{Module: "m1", State: "invalid", Reason: "no_mx", SkipReason: "smtp_unwired"}, chg("M1-05", "m1_own")},
		{Outcome{Module: "m1", State: "risky", Reason: "disposable", SkipReason: "smtp_unwired"}, chg("M1-11", "m1_own")},
		{Outcome{Module: "m1", State: "unknown", Reason: "dns_error", SkipReason: "smtp_unwired", FailureOrigin: "remote"}, chg("M1-20", "m1_own")},
		{Outcome{Module: "m1", State: "unknown", Reason: "dns_error", SkipReason: "smtp_unwired", FailureOrigin: "own"}, fre("M1-21")},
		{Outcome{Module: "m1", State: "risky", Reason: "plausible", SkipReason: "smtp_unwired", FromCache: true}, chg("M1-01", "m1_cache")},
	} {
		if got := Decide(c.in); !sameDecision(got, c.want) {
			t.Errorf("Decide(%+v) = {%s}, want {%s}", c.in, describe(got), describe(c.want))
		}
	}
	// the same token on a plausible answer is free whatever origin it carries (the origin is only read by an unknown)
	for _, og := range append(append([]string{}, originValues...), garbageOrigins...) {
		o := plausible("smtp_unwired")
		o.FailureOrigin = og
		if got := Decide(o); !sameDecision(got, fre("M1-17")) {
			t.Errorf("risky/plausible/smtp_unwired with origin %q: {%s}, want M1-17", og, describe(got))
		}
	}
}

// TestDecide_ExternalVerifiedIsTheVendorUnit: the premium unit, exactly as before the origin column existed. A vendor ANSWER carries the reason external_verified and is M1-02 (m1_vendor) for every
// status, skip token and failure origin, because M1-02 sits above every origin row and matches the reason, not the state: an `unknown` that carried external_verified would also be the
// vendor row's (module 1 never emits one: a vendor non-answer leaves M1's own result standing, so the ordinary rows charge it, never m1_vendor). A cached vendor answer is the cache's (M1-01).
func TestDecide_ExternalVerifiedIsTheVendorUnit(t *testing.T) {
	for _, st := range legalStatuses {
		for _, og := range append(append([]string{}, originValues...), garbageOrigins...) {
			for _, skip := range append([]string{"", "smtp_unwired"}, capacityTokens...) {
				o := Outcome{Module: "m1", State: st, Reason: "external_verified", SkipReason: skip, FailureOrigin: og}
				if got := Decide(o); !sameDecision(got, chg("M1-02", "m1_vendor")) {
					t.Errorf("%s/external_verified (skip %q, origin %q): {%s}, want M1-02 m1_vendor: the table must do what it did before the origin column", st, skip, og, describe(got))
				}
				o.FromCache = true
				if got := Decide(o); !sameDecision(got, chg("M1-01", "m1_cache")) {
					t.Errorf("a cached %s/external_verified (skip %q, origin %q): {%s}, want M1-01 m1_cache", st, skip, og, describe(got))
				}
			}
		}
	}
	// the controls: a vendor non-answer is M1's own result, charged (or not) by its ordinary row and never as vendor
	for _, c := range []struct {
		in   Outcome
		want Decision
	}{
		{m1("valid", "accepted"), chg("M1-03", "m1_own")},
		{m1("risky", "catch_all"), chg("M1-13", "m1_own")},
		{m1o("unknown", "smtp_timeout", "remote"), chg("M1-20", "m1_own")},
		{m1o("unknown", "smtp_timeout", "undetermined"), fre("M1-22")},
		{m1("unknown", "smtp_timeout"), unm("M1-23")},
	} {
		got := Decide(c.in)
		if !sameDecision(got, c.want) || got.Unit == "m1_vendor" {
			t.Errorf("Decide(%+v) = {%s}, want {%s} and never m1_vendor", c.in, describe(got), describe(c.want))
		}
	}
	// no row other than M1-02 charges the vendor unit
	for _, r := range Rules() {
		if r.Unit == "m1_vendor" && r.ID != "M1-02" {
			t.Errorf("row %s charges m1_vendor: only M1-02 may", r.ID)
		}
	}
}

// ---- module 2 and the platform ---------------------------------------------------------------------------------------------------------------------------------------------------------------------

// TestDecide_M2Failed: a failed item is OUR failure and free whatever the reason: the attempt cap or max age (B3, reason attempts_exhausted, M2-03) and pool exhaustion or no reason at all (M2-04).
// The reason frees only a `failed` item: it never changes another state's row. Every state module 2 defines has an explicit row.
func TestDecide_M2Failed(t *testing.T) {
	for _, c := range []struct {
		in   Outcome
		want Decision
	}{
		{m2("failed", "attempts_exhausted"), fre("M2-03")},
		{m2("failed", "pool_exhausted"), fre("M2-04")},
		{m2("failed", ""), fre("M2-04")},
		{m2("failed", "some_future_reason"), fre("M2-04")},
		{m2("failed", "Attempts_Exhausted"), fre("M2-04")},  // exact comparison: a wrong-case reason is the generic failed row, which is free as well
		{m2("failed", "attempts_exhausted "), fre("M2-04")}, // likewise with a trailing space
		// the controls: the same reason on another state keeps that state's own row
		{m2("sent", "attempts_exhausted"), chg("M2-01", "m2_accepted")},
		{m2("bounced", "attempts_exhausted"), chg("M2-02", "m2_bounced")},
		{m2("queued", "attempts_exhausted"), nfi("M2-05")},
		{m2("assigned", "attempts_exhausted"), nfi("M2-06")},
		{m2("sending", "attempts_exhausted"), nfi("M2-07")},
		{m2("cancelled", "attempts_exhausted"), fre("M2-08")},
		{m2("refunded", "attempts_exhausted"), unm("M2-99")},
	} {
		if got := Decide(c.in); !sameDecision(got, c.want) {
			t.Errorf("Decide(%+v) = {%s}, want {%s}", c.in, describe(got), describe(c.want))
		}
	}
	for _, st := range legalM2States {
		if got := Decide(m2(st, "")); got.Rule == "M2-99" || got.Rule == noRule {
			t.Errorf("the module 2 state %q has no explicit row (decided by %s)", st, got.Rule)
		}
	}
	// a failed item is never charged and never unmapped, for any reason, cache flag, skip token or origin
	for _, re := range []string{"", "attempts_exhausted", "pool_exhausted", "x", "*"} {
		for _, cached := range []bool{false, true} {
			if got := Decide(Outcome{Module: "m2", State: "failed", Reason: re, FromCache: cached, SkipReason: "audit_red", FailureOrigin: "remote"}); got.Kind != Free {
				t.Errorf("m2 failed/%q (cached %v): {%s}, want free", re, cached, describe(got))
			}
		}
	}
}

// TestDecide_M2Cancelled: an item cancelled before it was claimed (B4) is work not done: free (M2-08). The control: the platform's own `cancelled` is P-01 and module 1 has no such state.
func TestDecide_M2Cancelled(t *testing.T) {
	for _, re := range []string{"", "cancelled_by_user", "x"} {
		if got := Decide(m2("cancelled", re)); !sameDecision(got, fre("M2-08")) {
			t.Errorf("m2 cancelled/%q: {%s}, want free by M2-08", re, describe(got))
		}
	}
	if got := Decide(Outcome{Module: "m2", State: "cancelled", FromCache: true, SkipReason: "audit_red", FailureOrigin: "remote"}); !sameDecision(got, fre("M2-08")) {
		t.Errorf("m2 cancelled with other columns set: {%s}, want M2-08", describe(got))
	}
	for _, c := range []struct {
		in   Outcome
		want Decision
	}{
		{Outcome{Module: "platform", State: "cancelled"}, fre("P-01")},
		{m1("cancelled", ""), unm("M1-99")},
		{Outcome{Module: "m3", State: "cancelled"}, fre("M3-01")},
		{m2("Cancelled", ""), unm("M2-99")},
		{m2("canceled", ""), unm("M2-99")}, // the American spelling is not a state module 2 defines
	} {
		if got := Decide(c.in); !sameDecision(got, c.want) {
			t.Errorf("Decide(%+v) = {%s}, want {%s}", c.in, describe(got), describe(c.want))
		}
	}
}

// TestDecide_PlatformDuplicate: a repeat of an earlier row's address inside one M1 job is checked once and charged once, on the first row; the platform's adapter gives the repeats the state
// `duplicate`, which is free (P-07). The controls: no other module has the state, and a wrong-case spelling matches no row.
func TestDecide_PlatformDuplicate(t *testing.T) {
	for _, o := range []Outcome{
		{Module: "platform", State: "duplicate"},
		{Module: "platform", State: "duplicate", Reason: "valid", FromCache: true, SkipReason: "audit_red", FailureOrigin: "remote"}, // nothing else on the outcome can make it charge
	} {
		if got := Decide(o); !sameDecision(got, fre("P-07")) {
			t.Errorf("Decide(%+v) = {%s}, want free by P-07", o, describe(got))
		}
	}
	for _, c := range []struct {
		in   Outcome
		want Decision
	}{
		{m1("duplicate", ""), unm("M1-99")},
		{m2("duplicate", ""), unm("M2-99")},
		{Outcome{Module: "m3", State: "duplicate"}, fre("M3-01")},
		{Outcome{Module: "platform", State: "Duplicate"}, unm(noRule)},
		{Outcome{Module: "platform", State: "duplicate "}, unm(noRule)},
	} {
		if got := Decide(c.in); !sameDecision(got, c.want) {
			t.Errorf("Decide(%+v) = {%s}, want {%s}", c.in, describe(got), describe(c.want))
		}
	}
}

// TestDecide_EveryLegalM1PairIsRuled: every legal status/reason pair of module 1 is matched by an explicit row or by the explicit catch-all; the pairs the module cannot emit are
// Unmapped and never Charge; and no M1 row names a status or reason that is not in the vocabulary (a typo in the table would be a row nothing can reach).
func TestDecide_EveryLegalM1PairIsRuled(t *testing.T) {
	isLegal := func(list []string, v string) bool { return contains(list, v) }
	for _, r := range Rules() {
		if r.Module != "m1" {
			continue
		}
		if r.State != "*" && !isLegal(legalStatuses, r.State) {
			t.Errorf("row %s names status %q, which is not in module 1's vocabulary", r.ID, r.State)
		}
		if r.Reason != "*" && !isLegal(legalReasons, r.Reason) {
			t.Errorf("row %s names reason %q, which is not in module 1's vocabulary", r.ID, r.Reason)
		}
	}
	emitted := map[string]bool{}
	for _, p := range emittedM1Pairs {
		emitted[p] = true
		st, re, _ := strings.Cut(p, "/")
		if !isLegal(legalStatuses, st) || !isLegal(legalReasons, re) {
			t.Fatalf("emittedM1Pairs names %q, which is not a legal pair: the test's own list is wrong", p)
		}
	}
	for _, re := range neverEmittedM1Reasons {
		if !isLegal(legalReasons, re) {
			t.Fatalf("neverEmittedM1Reasons names %q, which is not in the vocabulary", re)
		}
	}
	// A pair with no origin: an emitted pair is decided by an explicit row; any other pair by the catch-all, except an `unknown`, which has its own explicit
	// row for "no origin named" (M1-23): unmapped as well, but by the row that says why.
	catchAllRule := func(st string) string {
		if st == "unknown" {
			return "M1-23"
		}
		return "M1-99"
	}
	explicit, catchAll := 0, 0
	for _, st := range legalStatuses {
		for _, re := range legalReasons {
			got := Decide(m1(st, re))
			if emitted[st+"/"+re] || re == "external_verified" {
				explicit++
				if got.Rule == "M1-99" || got.Rule == noRule {
					t.Errorf("%s/%s is emitted by the module but is decided by %s, not an explicit row", st, re, got.Rule)
				}
				continue
			}
			catchAll++
			if got.Kind != Unmapped || got.Rule != catchAllRule(st) {
				t.Errorf("%s/%s is not emitted by the module: Decide = {%s}, want unmapped by %s", st, re, describe(got), catchAllRule(st))
			}
		}
	}
	if want := len(legalStatuses) * len(legalReasons); explicit+catchAll != want {
		t.Fatalf("examined %d of %d legal pairs", explicit+catchAll, want)
	}
	if explicit < len(emittedM1Pairs) || catchAll < len(neverEmittedM1Reasons)*len(legalStatuses) {
		t.Fatalf("explicit=%d catch-all=%d: fewer than the emitted pairs / the never-emitted combinations the test names", explicit, catchAll)
	}
	// the never-emitted reasons, with every status and every skip token, uncached, with no origin: Unmapped, never Charge.
	for _, re := range neverEmittedM1Reasons {
		for _, st := range legalStatuses {
			for _, skip := range append([]string{""}, capacityTokens...) {
				got := Decide(Outcome{Module: "m1", State: st, Reason: re, SkipReason: skip})
				if got.Kind != Unmapped || got.Rule != catchAllRule(st) || got.Unit != "" {
					t.Errorf("%s/%s (skip %q): Decide = {%s}, want unmapped by %s and no unit", st, re, skip, describe(got), catchAllRule(st))
				}
			}
		}
	}
}

// ---- input hygiene ------------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestDecide_InputHygiene(t *testing.T) {
	none := unm(noRule)
	cases := []struct {
		name string
		in   Outcome
		want Decision
	}{
		{"the zero outcome", Outcome{}, none},
		{"an unknown module", Outcome{Module: "m4", State: "sent"}, none},
		{"a module in the wrong case", Outcome{Module: "M2", State: "sent"}, none},
		{"a module with a leading space", Outcome{Module: " m2", State: "sent"}, none},
		{"the module text *", Outcome{Module: "*", State: "sent"}, none},
		{"the empty module", Outcome{State: "sent", Reason: "accepted"}, none},
		{"an m2 state in the wrong case", Outcome{Module: "m2", State: "SENT"}, unm("M2-99")},
		{"an m2 state with a trailing space", Outcome{Module: "m2", State: "sent "}, unm("M2-99")},
		{"the m2 state text *", Outcome{Module: "m2", State: "*"}, unm("M2-99")},
		{"the empty m2 state", Outcome{Module: "m2"}, unm("M2-99")},
		{"the m1 state text * with a charging reason", m1("*", "accepted"), unm("M1-99")},
		{"an m1 reason in the wrong case", m1("valid", "Accepted"), unm("M1-99")},
		{"the m1 reason text *", m1("valid", "*"), unm("M1-99")},
		{"the empty m1 reason", m1("valid", ""), unm("M1-99")},
		{"an m1 status in the wrong case", m1("VALID", "accepted"), unm("M1-99")},
		{"the skip text none is not an absent skip reason", Outcome{Module: "m1", State: "risky", Reason: "plausible", SkipReason: "none"}, unm("M1-99")},
		{"the skip text capacity is not a capacity token", Outcome{Module: "m1", State: "risky", Reason: "plausible", SkipReason: "capacity"}, unm("M1-99")},
		{"the skip text smtp_unwired in the wrong case is not the token", Outcome{Module: "m1", State: "risky", Reason: "plausible", SkipReason: "SMTP_UNWIRED"}, unm("M1-99")},
		{"the origin text none is not an absent origin", m1o("unknown", "smtp_timeout", "none"), unm("M1-99")},
		{"the origin text * is not a wildcard", m1o("unknown", "smtp_timeout", "*"), unm("M1-99")},
		{"an origin in the wrong case", m1o("unknown", "smtp_timeout", "Remote"), unm("M1-99")},
		{"an origin with a trailing space", m1o("unknown", "greylisted", "remote "), unm("M1-99")},
		{"an origin with a leading space", m1o("unknown", "unknown_error", " own"), unm("M1-99")},
		{"an origin the module never emits", m1o("unknown", "smtp_error", "bogus"), unm("M1-99")},
		{"an m1 unknown in the wrong case still names no row", m1o("Unknown", "greylisted", "remote"), unm("M1-99")},
		{"an m1 reason in the wrong case with a remote origin", m1o("unknown", "Greylisted", "remote"), chg("M1-20", "m1_own")},
		{"a platform state the table does not know", Outcome{Module: "platform", State: "refunded"}, none},
		{"a platform state in the wrong case", Outcome{Module: "platform", State: "Cancelled"}, none},
		{"a platform duplicate in the wrong case", Outcome{Module: "platform", State: "Duplicate"}, none},
		{"m3 is free whatever it says", Outcome{Module: "m3", State: "whatever", Reason: "x", FromCache: true, SkipReason: "y", FailureOrigin: "remote"}, fre("M3-01")},
		{"m3 with nothing else", Outcome{Module: "m3"}, fre("M3-01")},
	}
	for _, c := range cases {
		got := Decide(c.in)
		if !sameDecision(got, c.want) {
			t.Errorf("%s: Decide(%+v) = {%s}, want {%s}", c.name, c.in, describe(got), describe(c.want))
		}
		if got.Kind == Charge && c.want.Kind != Charge {
			t.Errorf("%s: charged an outcome the table does not charge", c.name)
		}
	}
}

func TestDecide_PureAndDeterministic(t *testing.T) {
	before := Rules()
	for _, c := range rowCases {
		first := Decide(c.in)
		for i := 0; i < 5; i++ {
			if again := Decide(c.in); again != first {
				t.Fatalf("row %s: Decide changed between calls: {%s} then {%s}", c.id, describe(first), describe(again))
			}
		}
		if viaTable := decide(Rules(), c.in); viaTable != first {
			t.Errorf("row %s: Decide and decide(Rules()) disagree: {%s} vs {%s}", c.id, describe(first), describe(viaTable))
		}
	}
	if !reflect.DeepEqual(before, Rules()) {
		t.Error("deciding changed the table")
	}
}

// ---- the never-charge-by-accident property -------------------------------------------------------------------------------------------------------------------------------------------

type violation struct {
	kind   string
	o      Outcome
	detail string
}

const (
	vPanic            = "panic"
	vNondeterministic = "nondeterministic"
	vNoRule           = "no-rule-id"
	vBadKind          = "kind-invalid"
	vUnitUnknown      = "unit-unknown"
	vUnitModule       = "unit-module-mismatch"
	vForbiddenCharge  = "forbidden-module-charge"
	vUnitOnNonCharge  = "unit-on-non-charge"
	vUnknownModule    = "unknown-module-not-unmapped"
	vUnknownCharged   = "unknown-charged-without-a-remote-origin"
)

type sweepStats struct {
	examined int
	kinds    map[Kind]int
	charges  map[string]int // Charge decisions per module
}

func callSafely(fn func(Outcome) Decision, o Outcome) (d Decision, panicked any) {
	defer func() {
		if r := recover(); r != nil {
			panicked = r
		}
	}()
	return fn(o), nil
}

// sweepViolations is the checking helper of the property: it runs fn over every outcome of the domain and reports each way a decision could charge by accident.
func sweepViolations(fn func(Outcome) Decision, domain []Outcome) ([]violation, sweepStats) {
	st := sweepStats{kinds: map[Kind]int{}, charges: map[string]int{}}
	var out []violation
	add := func(kind string, o Outcome, format string, args ...any) {
		out = append(out, violation{kind: kind, o: o, detail: fmt.Sprintf(format, args...)})
	}
	for _, o := range domain {
		st.examined++
		d, p := callSafely(fn, o)
		if p != nil {
			add(vPanic, o, "the matcher panicked: %v", p)
			continue
		}
		if d2, _ := callSafely(fn, o); d2 != d {
			add(vNondeterministic, o, "two calls decided {%s} and {%s}", describe(d), describe(d2))
		}
		st.kinds[d.Kind]++
		switch d.Kind {
		case Charge, Free, NotFinal, Unmapped:
		default:
			add(vBadKind, o, "the kind %d is none of the four", int(d.Kind))
		}
		if d.Rule == "" {
			add(vNoRule, o, "the decision names no row and not %q either", noRule)
		}
		if !contains(modules, o.Module) && (d.Kind != Unmapped || d.Rule != noRule) {
			add(vUnknownModule, o, "an unknown module was decided {%s}", describe(d))
		}
		if d.Kind != Charge {
			if d.Unit != "" {
				add(vUnitOnNonCharge, o, "a %s decision carries the unit %q", d.Kind, d.Unit)
			}
			continue
		}
		st.charges[o.Module]++
		// The owner's unknown rule as a property: an M1 unknown is charged only when the recipient's server caused it (origin remote); a cached or a vendor answer is not an
		// unknown of this kind (its row is the cache's or the vendor's).
		if o.Module == "m1" && o.State == "unknown" && !o.FromCache && o.Reason != "external_verified" && o.FailureOrigin != "remote" {
			add(vUnknownCharged, o, "an unknown whose failure origin is %q (not remote) was charged {%s}", o.FailureOrigin, describe(d))
		}
		if !contains(Units(), d.Unit) {
			add(vUnitUnknown, o, "charges the unit %q, which is not one of the seeded units", d.Unit)
		} else if unitModule[d.Unit] != o.Module {
			add(vUnitModule, o, "module %q is charged the unit %q, which belongs to module %s", o.Module, d.Unit, unitModule[d.Unit])
		}
		if o.Module == "platform" || o.Module == "m3" {
			add(vForbiddenCharge, o, "module %q must never be charged, got {%s}", o.Module, describe(d))
		}
	}
	return out, st
}

func hasViolation(v []violation, kind, module string) bool {
	for _, x := range v {
		if x.kind == kind && (module == "" || x.o.Module == module) {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sweepDomain is every (module, state, reason, cache, skip, origin) combination drawn from the union of the values the table names plus three foreign values per column.
// It is sorted, so a run is the same on every machine.
func sweepDomain() []Outcome {
	mods, states, reasons, skips, origs := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range Rules() {
		mods[r.Module] = true
		if r.State != "*" {
			states[r.State] = true
		}
		if r.Reason != "*" {
			reasons[r.Reason] = true
		}
		if r.Skip != "*" && r.Skip != "none" && r.Skip != "capacity" {
			skips[r.Skip] = true
		}
		if r.Origin != "*" && r.Origin != "none" {
			origs[r.Origin] = true
		}
	}
	origs[""] = true // "none" is the absent origin
	origs["bogus"] = true
	skips[""] = true // "none" is the absent skip reason
	for _, tok := range capacityTokens {
		skips[tok] = true // "capacity" is any of these four
	}
	for _, v := range []string{"", "M1", "m4"} {
		mods[v] = true
	}
	for _, v := range []string{"", "*", "Sent"} {
		states[v] = true
	}
	for _, v := range []string{"", "*", "never_emitted_reason"} {
		reasons[v] = true
	}
	for _, v := range []string{"provider_mx_routed", "none", "capacity"} {
		skips[v] = true
	}
	var out []Outcome
	for _, m := range sortedKeys(mods) {
		for _, s := range sortedKeys(states) {
			for _, re := range sortedKeys(reasons) {
				for _, cached := range []bool{false, true} {
					for _, sk := range sortedKeys(skips) {
						for _, og := range sortedKeys(origs) {
							out = append(out, Outcome{Module: m, State: s, Reason: re, FromCache: cached, SkipReason: sk, FailureOrigin: og})
						}
					}
				}
			}
		}
	}
	return out
}

func TestDecide_NeverChargeByAccident(t *testing.T) {
	domain := sweepDomain()
	if len(domain) < 10000 {
		t.Fatalf("the sweep domain has %d outcomes: the instrument matched too little", len(domain))
	}
	viol, st := sweepViolations(func(o Outcome) Decision { return Decide(o) }, domain)
	for i, v := range viol {
		if i == 10 {
			t.Errorf("... and %d more violation(s)", len(viol)-10)
			break
		}
		t.Errorf("%s: %+v: %s", v.kind, v.o, v.detail)
	}
	// the sweep must have had something to find: a table that charges nothing, or never reaches Unmapped, would pass vacuously.
	if st.charges["m1"] == 0 || st.charges["m2"] == 0 {
		t.Errorf("the sweep saw no charge for m1 (%d) or m2 (%d): its subject is empty", st.charges["m1"], st.charges["m2"])
	}
	if st.kinds[Unmapped] == 0 || st.kinds[Free] == 0 || st.kinds[NotFinal] == 0 {
		t.Errorf("the sweep did not reach every non-charge kind: %v", st.kinds)
	}
	if st.charges["platform"] != 0 || st.charges["m3"] != 0 {
		t.Errorf("a charge for platform (%d) or m3 (%d)", st.charges["platform"], st.charges["m3"])
	}
	t.Logf("swept %d outcomes: charge=%d free=%d notfinal=%d unmapped=%d, charges per module %v", st.examined, st.kinds[Charge], st.kinds[Free], st.kinds[NotFinal], st.kinds[Unmapped], st.charges)
}

// TestDecide_SweepControl is the paired control of the sweep: the same checking helper, run on deliberately broken matchers, must return the violation each one earns. A helper
// that returned nothing for these would make TestDecide_NeverChargeByAccident a test that cannot fail.
func TestDecide_SweepControl(t *testing.T) {
	domain := sweepDomain()
	withRows := func(edit func(rules []Rule)) func(Outcome) Decision {
		rules := Rules()
		edit(rules)
		return func(o Outcome) Decision { return decide(rules, o) }
	}
	rowIndex := func(rules []Rule, id string) int {
		for i, r := range rules {
			if r.ID == id {
				return i
			}
		}
		t.Fatalf("row %s is not in the table", id)
		return -1
	}

	if v, st := sweepViolations(func(o Outcome) Decision { return Decide(o) }, domain); len(v) != 0 || st.examined != len(domain) {
		t.Fatalf("the helper reports %d violation(s) over %d of %d outcomes on the REAL table: the control proves nothing if the baseline is not clean", len(v), st.examined, len(domain))
	}

	// the lead's control: a first row that charges everything.
	charging := append([]Rule{{ID: "BROKEN-01", Module: "*", State: "*", Reason: "*", Cache: "*", Skip: "*", Origin: "*", Kind: Charge, Unit: "m1_own"}}, Rules()...)
	v, _ := sweepViolations(func(o Outcome) Decision { return decide(charging, o) }, domain)
	for _, want := range []struct{ kind, module string }{
		{vForbiddenCharge, "platform"}, {vForbiddenCharge, "m3"}, {vUnitModule, "m2"}, {vUnknownModule, "m4"}, {vUnknownCharged, "m1"},
	} {
		if !hasViolation(v, want.kind, want.module) {
			t.Errorf("a table whose first row is *|*|*|*|*|*|*|charge|m1_own: no %s violation for module %q (got %d violations)", want.kind, want.module, len(v))
		}
	}

	broken := []struct {
		name string
		fn   func(Outcome) Decision
		kind string
		mod  string
	}{
		{"a platform row that charges", withRows(func(r []Rule) { i := rowIndex(r, "P-04"); r[i].Kind, r[i].Unit = Charge, "m1_own" }), vForbiddenCharge, "platform"},
		{"an m3 row that charges", withRows(func(r []Rule) { i := rowIndex(r, "M3-01"); r[i].Kind, r[i].Unit = Charge, "m2_accepted" }), vForbiddenCharge, "m3"},
		{"a charge of a unit the database does not know", withRows(func(r []Rule) { r[rowIndex(r, "M2-01")].Unit = "m2_bogus" }), vUnitUnknown, "m2"},
		{"a charge of another module's unit", withRows(func(r []Rule) { r[rowIndex(r, "M2-02")].Unit = "m1_own" }), vUnitModule, "m2"},
		{"an unmapped row that carries a unit", withRows(func(r []Rule) { r[rowIndex(r, "M1-23")].Unit = "m1_own" }), vUnitOnNonCharge, "m1"},
		// the owner's unknown rule: a row that charges an unknown the recipient's server did not cause is a violation, whichever row it is
		{"an own-origin unknown row that charges", withRows(func(r []Rule) { i := rowIndex(r, "M1-21"); r[i].Kind, r[i].Unit = Charge, "m1_own" }), vUnknownCharged, "m1"},
		{"an undetermined-origin unknown row that charges", withRows(func(r []Rule) { i := rowIndex(r, "M1-22"); r[i].Kind, r[i].Unit = Charge, "m1_own" }), vUnknownCharged, "m1"},
		{"the no-origin unknown row that charges", withRows(func(r []Rule) { i := rowIndex(r, "M1-23"); r[i].Kind, r[i].Unit = Charge, "m1_own" }), vUnknownCharged, "m1"},
		{"the attempt-cap row that charges", withRows(func(r []Rule) { i := rowIndex(r, "M1-19"); r[i].Kind, r[i].Unit = Charge, "m1_own" }), vUnknownCharged, "m1"},
		{"the origin cell of the remote row widened to *", withRows(func(r []Rule) { r[rowIndex(r, "M1-20")].Origin = "*" }), vUnknownCharged, "m1"},
		{"an unknown module that matches a row", withRows(func(r []Rule) { r[rowIndex(r, "M3-01")].Module = "*" }), vUnknownModule, "m4"},
		{"a decision with no row", func(o Outcome) Decision { return Decision{Kind: Free} }, vNoRule, ""},
		{"a kind that is none of the four", func(o Outcome) Decision { return Decision{Rule: "X-01"} }, vBadKind, ""},
		{"a matcher that panics for m3", func(o Outcome) Decision {
			if o.Module == "m3" {
				panic("boom")
			}
			return Decide(o)
		}, vPanic, "m3"},
		{"a matcher that is not deterministic", func() func(Outcome) Decision {
			calls := 0
			return func(o Outcome) Decision {
				calls++
				d := Decide(o)
				if calls%2 == 0 {
					d.Rule = "X-02"
				}
				return d
			}
		}(), vNondeterministic, ""},
	}
	for _, b := range broken {
		viol, _ := sweepViolations(b.fn, domain)
		if !hasViolation(viol, b.kind, b.mod) {
			t.Errorf("%s: the sweep reports no %s violation for module %q (got %d violations): the property cannot fail", b.name, b.kind, b.mod, len(viol))
		}
	}
}

// ---- the parser refuses a broken table ----------------------------------------------------------------------------------------------------------------------------------------------------

func tableOf(lines ...string) string { return header + "\n" + strings.Join(lines, "\n") + "\n" }

var goodLines = []string{
	"X-01|m1|valid|accepted|no|*|*|charge|m1_own|a",
	"X-02|m1|invalid|no_mx|no|*|*|charge|m1_own|b",
	"X-03|m2|sent|*|*|*|*|charge|m2_accepted|c",
}

// withLine2 is the good table with its second row replaced.
func withLine2(row string) string { return tableOf(goodLines[0], row, goodLines[2]) }

func TestParse_RefusesMalformedTables(t *testing.T) {
	if _, err := parse(tableOf(goodLines...)); err != nil {
		t.Fatalf("the base table of this test is refused (%v): every refusal below would prove nothing", err)
	}
	if _, err := parse(strings.TrimSuffix(tableOf(goodLines...), "\n")); err != nil {
		t.Errorf("a table without a final newline is refused: %v", err)
	}
	if _, err := parse(strings.ReplaceAll(tableOf(goodLines...), "\n", "\r\n")); err != nil {
		t.Errorf("a table with CRLF line endings is refused: %v", err)
	}
	cases := []struct {
		name string
		src  string
		row  string // the row id the error must name ("" for an error about the table as a whole)
		frag string // the reason the error must give
	}{
		{"a missing column", withLine2("X-02|m1|invalid|no_mx|no|*|*|charge|m1_own"), "X-02", "has 9 columns, want 10"},
		{"a row of the old 9-column shape (no origin column)", withLine2("X-02|m1|invalid|no_mx|no|*|charge|m1_own|b"), "X-02", "has 9 columns, want 10"},
		{"two columns short", withLine2("X-02|m1|invalid|no_mx|no|*|charge|m1_own"), "X-02", "has 8 columns, want 10"},
		{"an extra column", withLine2("X-02|m1|invalid|no_mx|no|*|*|charge|m1_own|b|extra"), "X-02", "has 11 columns, want 10"},
		{"an unknown decision", withLine2("X-02|m1|invalid|no_mx|no|*|*|refund|m1_own|b"), "X-02", `unknown decision "refund"`},
		{"a charge without a unit", withLine2("X-02|m1|invalid|no_mx|no|*|*|charge||b"), "X-02", "a charge row needs a unit"},
		{"a free row with a unit", withLine2("X-02|m1|invalid|no_mx|no|*|*|free|m1_own|b"), "X-02", `decision "free" must not carry a unit`},
		{"a notfinal row with a unit", withLine2("X-02|m1|invalid|no_mx|no|*|*|notfinal|m1_own|b"), "X-02", `decision "notfinal" must not carry a unit`},
		{"an unmapped row with a unit", withLine2("X-02|m1|invalid|no_mx|no|*|*|unmapped|m1_own|b"), "X-02", `decision "unmapped" must not carry a unit`},
		{"a unit that is not seeded", withLine2("X-02|m1|invalid|no_mx|no|*|*|charge|m1_bogus|b"), "X-02", `unit "m1_bogus" is not one of the seeded units`},
		{"a seeded unit of the other module", tableOf(goodLines[0], goodLines[1], "X-03|m2|sent|*|*|*|*|charge|m1_own|c"), "X-03", `unit "m1_own" belongs to module m1, not m2`},
		{"a duplicate id", tableOf(goodLines[0], "X-01|m1|invalid|no_mx|no|*|*|charge|m1_own|b", goodLines[2]), "X-01", "duplicate id (first used on line 2)"},
		{"a wildcard module", withLine2("X-02|*|*|*|*|*|*|charge|m1_own|"), "X-02", "a wildcard module is refused"},
		{"an unknown module", withLine2("X-02|m4|invalid|no_mx|no|*|*|free||b"), "X-02", `module "m4" is not one of`},
		{"a bad cache value", withLine2("X-02|m1|invalid|no_mx|maybe|*|*|charge|m1_own|b"), "X-02", `the cache cell "maybe"`},
		{"an empty state cell", withLine2("X-02|m1||no_mx|no|*|*|charge|m1_own|b"), "X-02", "the state cell"},
		{"a partial wildcard", withLine2("X-02|m1|inv*|no_mx|no|*|*|charge|m1_own|b"), "X-02", "partial wildcard"},
		{"whitespace in a cell", withLine2("X-02|m1|invalid|no mx|no|*|*|charge|m1_own|b"), "X-02", "the reason cell"},
		{"a bad origin value", withLine2("X-02|m1|invalid|no_mx|no|*|bogus|charge|m1_own|b"), "X-02", `the origin cell "bogus" must be *, own, remote, undetermined, none`},
		{"an origin in the wrong case", withLine2("X-02|m1|invalid|no_mx|no|*|Remote|charge|m1_own|b"), "X-02", `the origin cell "Remote"`},
		{"an empty origin cell", withLine2("X-02|m1|invalid|no_mx|no|*||charge|m1_own|b"), "X-02", `the origin cell ""`},
		{"a partial wildcard in the origin cell", withLine2("X-02|m1|invalid|no_mx|no|*|re*|charge|m1_own|b"), "X-02", `the origin cell "re*"`},
		{"a bad id", withLine2("x-02|m1|invalid|no_mx|no|*|*|charge|m1_own|b"), "x-02", "the id must look like"},
		{"a row an earlier row covers entirely", tableOf("X-01|m1|*|*|*|*|*|unmapped||a", goodLines[1]), "X-02", "shadowed by the earlier row X-01"},
		{"a capacity token under an earlier capacity row", tableOf("X-01|m1|risky|plausible|*|capacity|*|free||a", "X-02|m1|risky|plausible|no|audit_red|*|charge|m1_own|b"), "X-02", "shadowed by the earlier row X-01"},
		{"an origin row under an earlier row of every origin", tableOf("X-01|m1|unknown|*|no|*|*|unmapped||a", "X-02|m1|unknown|smtp_timeout|no|*|remote|charge|m1_own|b"), "X-02", "shadowed by the earlier row X-01"},
		{"the same origin under an earlier wildcard reason", tableOf("X-01|m1|unknown|*|no|*|remote|charge|m1_own|a", "X-02|m1|unknown|smtp_timeout|no|*|remote|free||b"), "X-02", "shadowed by the earlier row X-01"},
		{"an absent-origin row under an earlier absent-origin row", tableOf("X-01|m1|unknown|*|no|*|none|unmapped||a", "X-02|m1|unknown|greylisted|no|*|none|free||b"), "X-02", "shadowed by the earlier row X-01"},
		{"a blank line inside the table", tableOf(goodLines[0], "", goodLines[2]), "?", "(line 3): has 1 columns"},
		{"an empty table", "", "", "the table is empty"},
		{"a table of whitespace", " \n\n", "", "the table is empty"},
		{"a header and no rows", header + "\n", "", "a header and no rows"},
		{"a wrong header", strings.Replace(tableOf(goodLines...), "|decision|", "|verdict|", 1), "", "the header must be exactly"},
		{"the old 9-column header", strings.Replace(tableOf(goodLines...), "|skip|origin|decision|", "|skip|decision|", 1), "", "the header must be exactly"},
		{"the origin column named something else", strings.Replace(tableOf(goodLines...), "|origin|", "|failure_origin|", 1), "", "the header must be exactly"},
	}
	for _, c := range cases {
		rules, err := parse(c.src)
		if err == nil {
			t.Errorf("%s: the table was accepted (%d rows)", c.name, len(rules))
			continue
		}
		if c.row != "" && !strings.Contains(err.Error(), "row "+c.row+" ") {
			t.Errorf("%s: the error does not name the row %s: %v", c.name, c.row, err)
		}
		if !strings.Contains(err.Error(), c.frag) {
			t.Errorf("%s: the table was refused, but not for the reason %q: %v", c.name, c.frag, err)
		}
	}
}

func TestParse_PanicNamesTheRow(t *testing.T) {
	panicOf := func(src string) (msg string) {
		defer func() {
			if r := recover(); r != nil {
				msg = fmt.Sprint(r)
			}
		}()
		mustParse(src)
		return ""
	}
	if msg := panicOf(tableOf(goodLines...)); msg != "" {
		t.Fatalf("a good table panicked: %s", msg)
	}
	for _, c := range []struct{ name, src, row string }{
		{"an unseeded unit", withLine2("X-02|m1|invalid|no_mx|no|*|*|charge|m1_bogus|b"), "X-02"},
		{"a first row that charges everything", tableOf("X-01|*|*|*|*|*|*|charge|m1_own|", goodLines[1]), "X-01"},
		{"a duplicate id", tableOf(goodLines[0], goodLines[0]), "X-01"},
		{"a bad origin", withLine2("X-02|m1|invalid|no_mx|no|*|bogus|charge|m1_own|b"), "X-02"},
	} {
		msg := panicOf(c.src)
		if msg == "" {
			t.Errorf("%s: a broken table did not panic: it would run", c.name)
			continue
		}
		if !strings.Contains(msg, "row "+c.row+" ") || !strings.Contains(msg, "must never run") {
			t.Errorf("%s: the panic does not name row %s: %s", c.name, c.row, msg)
		}
	}
}

// TestParse_OriginRows is the control of the origin refusals above: the shape the real table uses is ACCEPTED. A specific origin row is not shadowed by a later wildcard-origin row, the four
// origin values are four different rows of one outcome (not shadows of each other), and a more specific reason before a wildcard reason of the same origin is fine. Without this a parser that
// refused every origin cell would pass every refusal case.
func TestParse_OriginRows(t *testing.T) {
	rules, err := parse(tableOf(
		"X-01|m1|unknown|greylisted|no|*|remote|charge|m1_own|a",
		"X-02|m1|unknown|unknown_error|no|*|own|free||b",
		"X-03|m1|unknown|*|no|*|remote|charge|m1_own|c",
		"X-04|m1|unknown|*|no|*|own|free||d",
		"X-05|m1|unknown|*|no|*|undetermined|free||e",
		"X-06|m1|unknown|*|no|*|none|unmapped||f",
		"X-07|m1|*|*|*|*|*|unmapped||g",
	))
	if err != nil {
		t.Fatalf("the shape of the real table is refused: %v", err)
	}
	var got []string
	for _, r := range rules {
		got = append(got, r.ID+":"+r.Origin)
	}
	if want := "X-01:remote,X-02:own,X-03:remote,X-04:own,X-05:undetermined,X-06:none,X-07:*"; strings.Join(got, ",") != want {
		t.Errorf("the parsed origins are %s, want %s", strings.Join(got, ","), want)
	}
	// the control of the control: the same rows, with the wildcard-origin row FIRST, are refused as shadowed, naming the first row
	_, err = parse(tableOf(
		"X-07|m1|*|*|*|*|*|unmapped||g",
		"X-06|m1|unknown|*|no|*|none|unmapped||f",
	))
	if err == nil || !strings.Contains(err.Error(), "row X-06 ") || !strings.Contains(err.Error(), "shadowed by the earlier row X-07") {
		t.Errorf("a wildcard-origin row before an origin row must shadow it, got: %v", err)
	}
}

// ---- the embedded table -------------------------------------------------------------------------------------------------------------------------------------------------------------------

func TestRules_EmbeddedTableIsWellFormed(t *testing.T) {
	parsed, err := parse(rulesPSV)
	if err != nil {
		t.Fatalf("the embedded table does not parse: %v", err)
	}
	if !reflect.DeepEqual(parsed, Rules()) {
		t.Error("Rules() is not the parse of the embedded file")
	}
	if len(parsed) < 30 {
		t.Errorf("the embedded table has %d rows (floor 30)", len(parsed))
	}
	if !strings.HasSuffix(rulesPSV, "\n") || strings.HasSuffix(rulesPSV, "\n\n") {
		t.Error("rules.psv must end with exactly one newline")
	}
	if strings.Contains(rulesPSV, "\r") {
		t.Error("rules.psv contains a carriage return")
	}
	kinds := map[Kind]int{}
	last := map[string]Rule{}
	for _, r := range parsed {
		kinds[r.Kind]++
		last[r.Module] = r
		switch r.Kind {
		case Charge:
			if !contains(Units(), r.Unit) || !strings.HasPrefix(r.Unit, r.Module+"_") {
				t.Errorf("row %s charges the unit %q for module %s", r.ID, r.Unit, r.Module)
			}
			if r.Module == "platform" || r.Module == "m3" {
				t.Errorf("row %s charges module %s", r.ID, r.Module)
			}
		default:
			if r.Unit != "" {
				t.Errorf("row %s is %s and carries the unit %q", r.ID, r.Kind, r.Unit)
			}
		}
	}
	for _, k := range []Kind{Charge, Free, NotFinal, Unmapped} {
		if kinds[k] == 0 {
			t.Errorf("the table has no %s row", k)
		}
	}
	for _, m := range modules {
		if _, ok := last[m]; !ok {
			t.Errorf("the table has no row for module %s", m)
		}
	}
	// the last row of the two metered modules is the explicit catch-all: nothing a module can say falls off the end of its rows.
	for module, id := range map[string]string{"m1": "M1-99", "m2": "M2-99"} {
		r := last[module]
		if r.ID != id || r.Kind != Unmapped || r.State != "*" || r.Reason != "*" || r.Cache != "*" || r.Skip != "*" || r.Origin != "*" {
			t.Errorf("the last %s row is %s (%s), want the catch-all %s: unmapped on every column *", module, r.ID, r.Kind, id)
		}
	}
}

// TestRules_OnlyTheUnknownRowsReadTheOrigin pins "every existing row gets origin *": the origin column is read by exactly the six M1 rows that rule on an `unknown` (M1-18..M1-23), with the
// value each is for, in this order; every other row, the vendor, cache and capacity rows included, ignores it. A row that starts to read an origin (or an unknown row that stops) is red here.
func TestRules_OnlyTheUnknownRowsReadTheOrigin(t *testing.T) {
	want := map[string]string{"M1-18": "remote", "M1-19": "own", "M1-20": "remote", "M1-21": "own", "M1-22": "undetermined", "M1-23": "none"}
	seen := 0
	for _, r := range Rules() {
		w, reads := want[r.ID]
		if !reads {
			w = "*"
		} else {
			seen++
			if r.Module != "m1" || r.State != "unknown" || r.Cache != "no" {
				t.Errorf("row %s reads an origin but is %s|%s|cache %s, want m1|unknown|no", r.ID, r.Module, r.State, r.Cache)
			}
		}
		if r.Origin != w {
			t.Errorf("row %s has origin %q, want %q", r.ID, r.Origin, w)
		}
	}
	if seen != len(want) {
		t.Errorf("found %d of the %d origin rows in the table", seen, len(want))
	}
	// the six rows are consecutive and in order, and sit before M1-99 (so nothing else catches an unknown first except the cache and vendor rows, which are above them).
	var order []string
	for _, r := range Rules() {
		if _, ok := want[r.ID]; ok {
			order = append(order, r.ID)
		}
	}
	if got, wantOrder := strings.Join(order, ","), "M1-18,M1-19,M1-20,M1-21,M1-22,M1-23"; got != wantOrder {
		t.Errorf("the origin rows are %s, want %s", got, wantOrder)
	}
}

// TestRules_IdsAreUniqueAndOrdered: within a module the numeric parts of the ids rise by one from 01 (the catch-all 99 last), so a renumbering that leaves a hole or reorders rows is visible.
func TestRules_IdsAreUniqueAndOrdered(t *testing.T) {
	next := map[string]int{}
	seen := map[string]bool{}
	for _, r := range Rules() {
		if seen[r.ID] {
			t.Errorf("duplicate id %s", r.ID)
		}
		seen[r.ID] = true
		prefix, num, ok := strings.Cut(r.ID, "-")
		if !ok {
			t.Fatalf("id %s has no dash", r.ID)
		}
		var n int
		if _, err := fmt.Sscanf(num, "%d", &n); err != nil {
			t.Fatalf("id %s: %v", r.ID, err)
		}
		if n == 99 {
			next[prefix] = 100
			continue
		}
		if next[prefix] == 0 {
			next[prefix] = 1
		}
		if n != next[prefix] {
			t.Errorf("id %s: the next id of %s is %02d (ids rise by one, the catch-all 99 is last)", r.ID, prefix, next[prefix])
		}
		next[prefix] = n + 1
	}
	if len(seen) < 30 {
		t.Errorf("only %d ids examined (floor 30)", len(seen))
	}
}

func TestUnitsMatchTheSeededUnitCosts(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", "platform_0002_registry_and_money.up.sql"))
	if err != nil {
		t.Fatalf("reading the migration that seeds platform.unit_costs: %v", err)
	}
	text := strings.ReplaceAll(string(b), "\r", "")
	i := strings.Index(text, "INSERT INTO platform.unit_costs")
	if i < 0 {
		t.Fatal("platform_0002 has no INSERT INTO platform.unit_costs: the instrument matched nothing")
	}
	stmt := text[i:]
	if j := strings.Index(stmt, ";"); j >= 0 {
		stmt = stmt[:j]
	}
	var seeded, owners []string
	for _, m := range regexp.MustCompile(`\(\s*'([a-z0-9_]+)'\s*,\s*'([a-z0-9_]+)'`).FindAllStringSubmatch(stmt, -1) {
		seeded, owners = append(seeded, m[1]), append(owners, m[2])
	}
	if !reflect.DeepEqual(seeded, Units()) {
		t.Errorf("the units seeded by platform_0002 are %v, Units() says %v", seeded, Units())
	}
	for i, u := range seeded {
		if unitModule[u] != owners[i] {
			t.Errorf("unit %s belongs to module %s in the migration and %s in the package", u, owners[i], unitModule[u])
		}
	}
}

func TestRulesAndUnits_ReturnCopies(t *testing.T) {
	r := Rules()
	r[0].Unit, r[0].Kind = "m2_bounced", Free
	r[len(r)-1].ID = "ZZ-99"
	if again := Rules(); again[0].Unit != "m1_cache" || again[0].Kind != Charge || again[len(again)-1].ID != "P-07" {
		t.Errorf("mutating the slice Rules() returned changed the table: %+v ... %+v", again[0], again[len(again)-1])
	}
	if got := Decide(m1("valid", "accepted")); !sameDecision(got, chg("M1-03", "m1_own")) {
		t.Errorf("mutating Rules()'s copy changed what Decide decides: {%s}", describe(got))
	}
	u := Units()
	if len(u) != 5 {
		t.Fatalf("Units() has %d entries, want the five seeded units", len(u))
	}
	u[0] = "hacked"
	if Units()[0] != "m1_own" {
		t.Error("mutating the slice Units() returned changed the package's units")
	}
}

func TestKind_String(t *testing.T) {
	for k, want := range map[Kind]string{Charge: "charge", Free: "free", NotFinal: "notfinal", Unmapped: "unmapped", Kind(0): "kind(0)", Kind(9): "kind(9)"} {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(k), got, want)
		}
	}
	var zero Decision
	if zero.Kind == Charge || zero.Kind.String() == "charge" {
		t.Error("the zero Decision reads as a Charge")
	}
}

// TestMatcher_ColumnSemantics pins the matcher's reading of the cache and skip columns on a small injected table, including the exact-token form the real table does not use.
func TestMatcher_ColumnSemantics(t *testing.T) {
	rules, err := parse(tableOf(
		"T-01|m1|*|*|*|exact_tok|*|free||an exact skip token, cached or not",
		"T-02|m1|*|*|yes|capacity|*|unmapped||cached, any capacity token",
		"T-03|m1|*|*|no|none|*|notfinal||uncached, no skip token",
		"T-04|m1|*|*|*|*|*|charge|m1_own|everything else of m1",
	))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		cached bool
		skip   string
		want   string
	}{
		{false, "exact_tok", "T-01"}, {true, "exact_tok", "T-01"}, {false, "exact_tok2", "T-04"}, {false, "Exact_Tok", "T-04"}, {false, " exact_tok", "T-04"},
		{true, "no_eligible_node", "T-02"}, {true, "audit_red", "T-02"}, {true, "warmup_cap", "T-02"}, {true, "rate_limited", "T-02"},
		{true, "", "T-04"}, {true, "provider_mx_routed", "T-04"}, {false, "", "T-03"}, {false, "rate_limited", "T-04"}, {false, "*", "T-04"}, {false, "none", "T-04"},
	}
	for _, c := range cases {
		o := Outcome{Module: "m1", State: "valid", Reason: "accepted", FromCache: c.cached, SkipReason: c.skip}
		if got := decide(rules, o); got.Rule != c.want {
			t.Errorf("cached=%v skip=%q: decided by %s, want %s", c.cached, c.skip, got.Rule, c.want)
		}
	}
	if got := decide(rules, Outcome{Module: "m2", State: "sent"}); got.Kind != Unmapped || got.Rule != noRule {
		t.Errorf("a module with no row: {%s}, want unmapped by %q", describe(got), noRule)
	}
}

// TestMatcher_OriginSemantics pins the matcher's reading of the origin column on a small injected table: `none` is the absent origin and only that, own / remote / undetermined are exact,
// `*` is a wildcard only in the table, and a row that is more specific than a later wildcard row is not shadowed by it (the order is what decides). Each case has its paired control in the
// same list (the same outcome with the origin changed).
func TestMatcher_OriginSemantics(t *testing.T) {
	rules, err := parse(tableOf(
		"T-01|m1|unknown|*|no|*|remote|charge|m1_own|remote",
		"T-02|m1|unknown|*|no|*|own|free||own",
		"T-03|m1|unknown|*|no|*|undetermined|notfinal||undetermined",
		"T-04|m1|unknown|*|no|*|none|unmapped||no origin named",
		"T-05|m1|*|*|*|*|*|free||everything else of m1, any origin",
	))
	if err != nil {
		t.Fatalf("the injected table is refused: %v", err)
	}
	cases := []struct {
		origin string
		want   string
	}{
		{"remote", "T-01"}, {"own", "T-02"}, {"undetermined", "T-03"}, {"", "T-04"},
		{"Remote", "T-05"}, {"OWN", "T-05"}, {"remote ", "T-05"}, {" remote", "T-05"}, {"undetermined\n", "T-05"}, {"none", "T-05"}, {"*", "T-05"}, {"bogus", "T-05"}, {"remote|own", "T-05"},
	}
	for _, c := range cases {
		o := Outcome{Module: "m1", State: "unknown", Reason: "smtp_timeout", FailureOrigin: c.origin}
		if got := decide(rules, o); got.Rule != c.want {
			t.Errorf("origin %q: decided by %s, want %s", c.origin, got.Rule, c.want)
		}
		// the same origin on a status the origin rows do not rule on is the catch-all's, never an origin row's
		if got := decide(rules, Outcome{Module: "m1", State: "valid", Reason: "accepted", FailureOrigin: c.origin}); got.Rule != "T-05" {
			t.Errorf("a valid outcome with origin %q: decided by %s, want the catch-all T-05", c.origin, got.Rule)
		}
	}
	// a cached unknown does not match the uncached origin rows, whatever its origin
	for _, origin := range []string{"remote", "own", "undetermined", ""} {
		if got := decide(rules, Outcome{Module: "m1", State: "unknown", Reason: "smtp_timeout", FromCache: true, FailureOrigin: origin}); got.Rule != "T-05" {
			t.Errorf("a cached unknown with origin %q: decided by %s, want the catch-all T-05", origin, got.Rule)
		}
	}
}

// ---- the table in the spec is the table in the code ----------------------------------------------------------------------------------------------------------------------------

// TestRulesFileMatchesSpec duplicates scripts/check-charge-table.sh on purpose: this one runs in the `go` workflow, the gate in `gates`. The fenced block between the markers in
// Docs/phase-2-spec.md must equal rules.psv (the file the package embeds) byte for byte.
func TestRulesFileMatchesSpec(t *testing.T) {
	const begin, end = "<!-- charge-table:begin -->", "<!-- charge-table:end -->"
	b, err := os.ReadFile(filepath.Join("..", "..", "Docs", "phase-2-spec.md"))
	if err != nil {
		t.Fatalf("reading the spec: %v", err)
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r", ""), "\n")
	var begins, ends []int
	for i, l := range lines {
		switch l {
		case begin:
			begins = append(begins, i)
		case end:
			ends = append(ends, i)
		}
	}
	if len(begins) != 1 || len(ends) != 1 {
		t.Fatalf("the spec has %d begin marker(s) and %d end marker(s): exactly one of each is required", len(begins), len(ends))
	}
	if begins[0] >= ends[0] {
		t.Fatalf("the end marker (line %d) is not after the begin marker (line %d)", ends[0]+1, begins[0]+1)
	}
	region := lines[begins[0]+1 : ends[0]]
	var fences []int
	for i, l := range region {
		if strings.HasPrefix(l, "```") {
			fences = append(fences, i)
		}
	}
	if len(fences) != 2 {
		t.Fatalf("the region between the markers has %d fence line(s): exactly one fenced block (two fence lines) is required", len(fences))
	}
	for i, l := range region {
		if (i < fences[0] || i > fences[1]) && strings.TrimSpace(l) != "" {
			t.Fatalf("the region between the markers has text outside the fenced block: %q", l)
		}
	}
	block := strings.Join(region[fences[0]+1:fences[1]], "\n") + "\n"
	if block == "\n" {
		t.Fatal("the fenced block between the markers is empty")
	}
	if block == rulesPSV {
		return
	}
	got, want := strings.Split(rulesPSV, "\n"), strings.Split(block, "\n")
	for i := 0; i < len(got) || i < len(want); i++ {
		var g, w string
		if i < len(got) {
			g = got[i]
		}
		if i < len(want) {
			w = want[i]
		}
		if g != w {
			t.Fatalf("rules.psv differs from the spec block at line %d:\n  spec:  %q\n  rules: %q", i+1, w, g)
		}
	}
	t.Fatal("rules.psv differs from the spec block, but every line is equal")
}

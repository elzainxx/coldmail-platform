package sendapi

import (
	"errors"
	"fmt"
	"slices"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// Class says what an item's state means for settlement. It is decided by stateClass, in one place, from the module's state spelling; a Profile cannot give a state a meaning of its own.
type Class uint8

const (
	// ClassPending: queued, not claimed yet or waiting to retry. Not an outcome.
	ClassPending Class = iota + 1
	// ClassInFlight: assigned or sending. Not an outcome.
	ClassInFlight
	// ClassTerminal: sent, bounced, failed or cancelled. Only a terminal item settles.
	ClassTerminal
)

// String is the word of the class.
func (c Class) String() string {
	switch c {
	case ClassPending:
		return "pending"
	case ClassInFlight:
		return "in flight"
	case ClassTerminal:
		return "terminal"
	}
	return fmt.Sprintf("class(%d)", uint8(c))
}

// stateClass is the adapter's switch over the item states of modules 2 and 3 (research m2 4.3, m3 5): the one place that says which states are final. A state it does not name is
// not guessed at (a module that adds a state must be looked at by a person, because whether the new state is final, and what it costs, is not something to infer from its name): the
// callers refuse it as a contract violation. The test TestStateSwitchIsTheUnionOfTheVocabularies keeps this list equal to the union of the profiles' JobStates.
func stateClass(state string) (Class, bool) {
	switch state {
	case "queued":
		return ClassPending, true
	case "assigned", "sending":
		return ClassInFlight, true
	case "sent", "bounced", "failed", "cancelled":
		return ClassTerminal, true
	}
	return 0, false
}

// Conflict says what a 409 on `run` means for a module.
type Conflict uint8

const (
	// ConflictBackpressure: module 2. Its 409 is code `conflict`, a fixed sentence and NO details (owner ruling OD-5: back-pressure, phase 4 owns the serialisation). The only cause that
	// matters is "this user has another campaign in flight" (a lost unique-violation race is the other, and it is a retry away from the same answer), so a 409 is back-pressure:
	// ErrBackpressure.
	ConflictBackpressure Conflict = iota + 1
	// ConflictCauses: module 3 since Phase 3 close-out C2. Its 409 keeps status, code and message and adds `details[]`, one entry per cause that applies (suspended, campaign_active,
	// campaign_cap, queue_cap): the error is a *ConflictError carrying exactly the causes the module named (errors.Is on ErrRunSuspended and the other three). A 409 that names none (a
	// module older than C2) is ErrConflictUnexplained, never a guess; a cause outside the closed set is a Contract error.
	ConflictCauses
)

// Profile is everything that differs between module 2 and module 3 on the four routes they answer identically (research m3-api section 11.2). It is data: the client body has no `if
// module == ...` anywhere. A Profile is validated by New; build one with the module's own package (m2.Profile()).
type Profile struct {
	// Module is the module the profile is for (modclient.M2 or modclient.M3). It is also the charge table's module column ("m2", "m3").
	Module modclient.Module
	// DescribeModule is the `module` the module's describe names ("send_managed", "send_byo"): a describe that names another is a wiring mistake.
	DescribeModule string
	// JobStates is the closed set of item states the module can report (describe vocabularies.job_state, in the module's enum order). Every one must be a state stateClass handles. A
	// profile that lists `cancelled` also has a `cancelled` count in the campaign totals (module 2, and module 3 since Phase 3 close-out C1). A state outside this set is a contract violation.
	JobStates []string
	// ErrorClasses is the closed set of error_class values (describe vocabularies.error_class). A value outside it is tolerated and carried (the set is additive).
	ErrorClasses []string
	// ItemReasons is the closed set of `reason` values (module 2's describe vocabularies.item_reason); empty when the module has no `reason` key (module 3). A value outside it is tolerated
	// and carried; the charge table reads it.
	ItemReasons []string
	// ErrorCodes is the set of error.code values the module's Domain A emits (describe error_codes): module 2 seven, module 3 nine.
	ErrorCodes []string
	// AlwaysPresent is true when the single-job report carries `error_class` (and `reason`) as keys, null when there is nothing to say (module 2), false when it omits them until set (module
	// 3). The campaign items always carry every key, null when unset, on both.
	AlwaysPresent bool
	// AccountDetail is true for module 3: its items carry smtp_response, failed_at, account_id and account_username, and its campaign report carries totals_by_account.
	AccountDetail bool
	// QuoteFeasibility is true for module 3: its quote answers `feasible` and `reasons[]` besides the price.
	QuoteFeasibility bool
	// LinearCost is true when the quote's cost must equal recipient_count * unit_price_credits (module 2; module 3's cost is inert).
	LinearCost bool
	// Cancel is true when the module has POST /v1/cancel (module 2; module 3 since Phase 3 close-out C1): Client.Cancel works, and Contracts lists the cancel responses. A profile with Cancel
	// lists the `cancelled` state, the one a cancelled item lands in.
	Cancel bool
	// RunConflict says what a 409 on run means.
	RunConflict Conflict
}

// The vocabulary names are describe's own (vocabularies.<name>), so the live drift gate compares like with like.
const (
	vocabJobState   = "job_state"
	vocabErrorClass = "error_class"
	vocabItemReason = "item_reason"
	vocabErrorCodes = "error_codes"
	// vocabRunConflictCause is the closed set of causes of a run 409 (module 3's describe vocabulary of that name); the adapter owns the list (runCauses) because the sentinels are its own.
	vocabRunConflictCause = "run_conflict_cause"
	apiVersion            = "v1"
	maxVocabularyLen      = 64
)

// strictVocabularies are the vocabularies a value outside which is a contract violation at run time (not merely an additive change): the item state decides what is final and what is
// charged, and the cause of a run 409 decides whether the platform stops, waits or splits the campaign, so a new state or cause is never mapped by guesswork.
var strictVocabularies = map[string]bool{vocabJobState: true, vocabRunConflictCause: true}

// hasCancelled reports whether `cancelled` is a state of the module (and a key of its campaign totals).
func (p Profile) hasCancelled() bool { return slices.Contains(p.JobStates, "cancelled") }

// hasReason reports whether the module's items carry a `reason` key.
func (p Profile) hasReason() bool { return len(p.ItemReasons) > 0 }

// route is the label of a call of this module ("m2.quote"): lower-case, the module's own prefix.
func (p Profile) route(op string) string { return string(p.Module) + "." + op }

// Vocabularies are the closed sets the profile's adapter maps from, by describe's names. The returned map and its slices are copies.
func (p Profile) Vocabularies() map[string][]string {
	out := map[string][]string{
		vocabJobState:   slices.Clone(p.JobStates),
		vocabErrorClass: slices.Clone(p.ErrorClasses),
		vocabErrorCodes: slices.Clone(p.ErrorCodes),
	}
	if p.hasReason() {
		out[vocabItemReason] = slices.Clone(p.ItemReasons)
	}
	if p.RunConflict == ConflictCauses {
		out[vocabRunConflictCause] = slices.Clone(runCauses)
	}
	return out
}

func (p Profile) clone() Profile {
	p.JobStates = slices.Clone(p.JobStates)
	p.ErrorClasses = slices.Clone(p.ErrorClasses)
	p.ItemReasons = slices.Clone(p.ItemReasons)
	p.ErrorCodes = slices.Clone(p.ErrorCodes)
	return p
}

func (p Profile) hasState(s string) bool { return slices.Contains(p.JobStates, s) }

// validate refuses a profile that could make the client invent something: an unknown module, a state the switch does not handle, a set with a duplicate or an empty value, no terminal state.
func (p Profile) validate() error {
	if p.Module != modclient.M2 && p.Module != modclient.M3 {
		return errors.New("the profile's module must be m2 or m3")
	}
	if !safeName(p.DescribeModule) {
		return errors.New("the profile must name the module its describe answers")
	}
	switch p.RunConflict {
	case ConflictBackpressure, ConflictCauses:
	default:
		return errors.New("the profile must say what a 409 on run means")
	}
	sets := []struct {
		name   string
		values []string
		need   bool
	}{
		{vocabJobState, p.JobStates, true}, {vocabErrorClass, p.ErrorClasses, true}, {vocabItemReason, p.ItemReasons, false}, {vocabErrorCodes, p.ErrorCodes, true},
	}
	for _, s := range sets {
		if s.need && len(s.values) == 0 {
			return fmt.Errorf("the profile's %s set is empty", s.name)
		}
		seen := map[string]bool{}
		for _, v := range s.values {
			if !safeName(v) {
				return fmt.Errorf("the profile's %s set has a value that is not a lower-case word", s.name)
			}
			if seen[v] {
				return fmt.Errorf("the profile's %s set lists a value twice", s.name)
			}
			seen[v] = true
		}
	}
	terminal := 0
	for _, s := range p.JobStates {
		c, ok := stateClass(s)
		if !ok {
			return errors.New("the profile lists a job state the adapter's switch does not handle (the adapter never guesses what a state means)")
		}
		if c == ClassTerminal {
			terminal++
		}
	}
	if terminal == 0 {
		return errors.New("the profile lists no terminal state")
	}
	if p.Cancel && !p.hasCancelled() {
		return errors.New("the profile has a cancel route and no cancelled state for a cancelled item to land in")
	}
	return nil
}

// safeName accepts the alphabet of a module's fixed identifiers (lower-case letters, digits and underscore, starting with a letter), at most maxVocabularyLen bytes.
func safeName(s string) bool {
	if s == "" || len(s) > maxVocabularyLen || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

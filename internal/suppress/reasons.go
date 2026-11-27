package suppress

import (
	"errors"

	"github.com/coldmail-platform/platform/internal/norm"
)

// THE CLOSED VOCABULARY OF AN ADDRESS THAT WAS NOT WRITTEN. A user is told why a line of an import (or an address they typed) was not suppressed in one of these words and in nothing else: never the address,
// never a message of the validator or of the database. The first eleven are the platform's own address rule (norm.Reasons: norm/v1, then module 1's syntax layer), the last three are this package's.
// The set only grows: a word is never renamed or repurposed, and a test holds this list equal to norm's.
const (
	// ReasonCannotStore: the address passed the validator and the table cannot hold it (a NUL, white space in the local part, an empty label, more than 254 bytes: ErrCannotStore).
	ReasonCannotStore = "cannot_store"
	// ReasonBadLine: the line is not an address problem but a line problem: it is not valid CSV, or it has no cell for the email column.
	ReasonBadLine = "bad_line"
	// ReasonInvalidAddress: the address is refused by the rule for a reason this list does not name (never observed: the rule's reasons are closed).
	ReasonInvalidAddress = "invalid_address"
)

// Reasons returns every word of the vocabulary, in a stable order: norm's reasons that a user can cause, then this package's.
func Reasons() []string {
	var out []string
	for _, r := range norm.Reasons() {
		if r != norm.ReasonInternal { // "a library panicked on this input": never observed, and told as invalid_address
			out = append(out, string(r))
		}
	}
	for _, r := range norm.SyntaxReasons() {
		out = append(out, string(r))
	}
	return append(out, ReasonCannotStore, ReasonBadLine, ReasonInvalidAddress)
}

// ReasonOf is the word of the vocabulary for an error of this package: the reason of a refused address (an *InvalidAddressError, or a norm error), ReasonCannotStore for ErrCannotStore, and
// ReasonInvalidAddress for any other refusal of the address rule whose reason is not a word of the list. It is "" for nil and for an error that is not a refusal of an address.
func ReasonOf(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrCannotStore) {
		return ReasonCannotStore
	}
	if r, ok := norm.ReasonAny(err); ok {
		for _, w := range Reasons() {
			if string(r) == w {
				return w
			}
		}
		return ReasonInvalidAddress
	}
	var bad *InvalidAddressError
	if errors.As(err, &bad) {
		return ReasonInvalidAddress
	}
	return ""
}

package auth

import (
	"errors"
	"sync/atomic"
)

// ErrKeysUnavailable says the platform could not obtain the keys it needed to judge the token: the key set could not be fetched (the endpoint is down, times out, answers an error or
// something that is not a key set, or its certificate is not accepted) and nothing usable was held. It is a statement about the PLATFORM and never about the token: the token is neither
// accepted nor refused, because the platform cannot tell. The API answers it 503 `auth_unavailable`, where every error below it (ErrKey included) is the one 401.
//
// It is deliberately NOT an ErrKey (errors.Is(ErrKeysUnavailable, ErrKey) is false and the other way too): a caller that tests for ErrKey to say "this token is bad" must not catch it.
// What a Verifier returns for it is a *KeysUnavailableError, which carries a class word for a log and the stretch of unavailability it belongs to.
var ErrKeysUnavailable = errors.New("auth: the signing keys cannot be obtained")

// The class of a key set that could not be had: a closed list of words, and the one thing about the cause that may reach a log or a metric. They are never a URL, a host, a key id, a status
// text or any text of a response.
const (
	ClassUnreachable = "unreachable" // the request failed before any answer: refused, no route, a name that does not resolve, a reset
	ClassTimeout     = "timeout"     // no answer in time
	ClassCertificate = "certificate" // the server's certificate was not accepted
	ClassStatus      = "bad_status"  // the server answered something other than 200 (a redirect included: redirects are not followed)
	ClassUnreadable  = "unreadable"  // the answer could not be read, or is larger than MaxKeySetBytes
	ClassBadKeySet   = "bad_key_set" // the answer is not a key set, or holds no key this package can use
)

// stretch is one run of unavailability of a key set: it opens with the first failed fetch after a good one (or since the start) and closes with the next good fetch. Every
// *KeysUnavailableError made while it is open shares it, so that "the first error of this stretch" is a question with one answer however many requests, and however many failed fetches,
// the stretch holds. The API warns once for it (KeysUnavailableError.FirstOfStretch).
type stretch struct{ reported atomic.Bool }

// KeysUnavailableError is what Verify returns when the keys cannot be had (errors.Is(err, ErrKeysUnavailable)). Its text is a fixed sentence: no URL, no host, no key id, no part of the
// response and nothing of the token.
type KeysUnavailableError struct {
	class  string
	reason string
	run    *stretch
}

func (e *KeysUnavailableError) Error() string { return ErrKeysUnavailable.Error() + ": " + e.reason }
func (e *KeysUnavailableError) Unwrap() error { return ErrKeysUnavailable }

// Class is one of the Class words: why the key set could not be had.
func (e *KeysUnavailableError) Class() string { return e.class }

// FirstOfStretch is true for exactly one call over all the errors of one stretch of unavailability (the first one made, by whichever goroutine asks first), and false for every other call
// on any error of that stretch. A stretch ends when the key set is fetched again, so the next outage is a new stretch and its first error answers true again. It is how a caller logs an
// outage once, and not once per request. An error that belongs to no stretch (a *KeysUnavailableError that was not made by a Verifier) answers true every time: a repeated line is better
// than a lost one.
func (e *KeysUnavailableError) FirstOfStretch() bool {
	if e == nil || e.run == nil {
		return true
	}
	return e.run.reported.CompareAndSwap(false, true)
}

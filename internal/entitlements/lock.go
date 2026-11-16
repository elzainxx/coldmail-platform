package entitlements

import (
	"hash/fnv"
	"strings"
)

// The platform's advisory-lock band is 7_xxx_xxx_xxx and no other (CLAUDE.md rule 9; module 3 holds 8_, module
// 2 holds 9_). The money store takes `7_000_000_000 + fnv1a64("<user>|<module>") mod 10^9` first in every
// transaction that writes one user's rows of one module; the entitlement writers (enable, disable, expiry)
// take the same kind of key for the user's module-3 rows, so that all writers of one (user, module) serialise.
//
// The function is written out here and is NOT imported from internal/money: the two packages agree on the
// number by construction (and a test in each pins the same vectors), not by a dependency.
const (
	lockBand      int64 = 7_000_000_000
	lockBandWidth       = 1_000_000_000
	moduleM3            = "m3"
)

// lockKey is the advisory-lock key of the (user, module) pair.
func lockKey(user, module string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(user + "|" + module))
	return lockBand + int64(h.Sum64()%lockBandWidth)
}

// LockKey is the advisory-lock key that serialises every writer of the user's M3 entitlement (enable,
// disable and the expiry), for a test that must hold exactly that lock. The id is canonicalised (lower case)
// first, so the key does not depend on how the caller spelled the uuid.
func LockKey(userID string) int64 { return lockKey(strings.ToLower(userID), moduleM3) }

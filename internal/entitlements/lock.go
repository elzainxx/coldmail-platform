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

// reservedKeys are the platform's own constant keys in the band: the migrator's (internal/migrate, written out here and not imported, the packages agree by construction) and the
// loop leader's. No (user, module) key is ever equal to one of them: bandKey steps over them (Phase 2 close-out D9, so that the leader's session lock can never make a user's writers wait).
var reservedKeys = [...]int64{7_100_000_001, LeaderLockKey}

// lockKey is the advisory-lock key of the (user, module) pair.
func lockKey(user, module string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(user + "|" + module))
	return bandKey(h.Sum64())
}

// bandKey reduces a hash into the band and steps over the reserved constant keys (one step is enough: the reserved keys are not adjacent). A hash that lands on a reserved key
// shares the key of the next number with whoever else hashes there: two users sharing one lock only serialise, which is what the lock is for.
func bandKey(h uint64) int64 {
	k := lockBand + int64(h%lockBandWidth)
	for _, r := range reservedKeys {
		if k == r {
			return k + 1
		}
	}
	return k
}

// LeaderLockKey is the SESSION-level advisory lock that makes ONE platform instance the runner of the expiry and reconcile loops (Phase 2 close-out D9; leader.go). It is a constant in the
// platform band, not a hash: the loops of every instance contend for this one number, and the harness (tests/harness/run.sh stage 9c, TestLeader_*) reads it from pg_locks.
//
// What else lives in the band: the migrator holds 7_100_000_001 while it runs (internal/migrate), and the money store and the entitlement writers take a TRANSACTION-level key per (user, module),
// hashed into 7_000_000_000..7_999_999_999 by lockKey above. A per-user key can NEVER equal this number or the migrator's: bandKey steps over the reserved constant keys (reservedKeys here and in
// internal/money, one test in each), so a session lock held for as long as an instance leads cannot make any user's writers wait. (Before that step the chance was one in 10^9 per user; it is zero now.)
//
// The lock is taken with pg_try_advisory_lock on ONE dedicated connection that the leader keeps for as long as it leads; PostgreSQL releases it the moment that connection or the process dies,
// which is what makes "a second instance takes over when the leader dies" true without a lease or a heartbeat.
const LeaderLockKey int64 = 7_200_000_002

// LockKey is the advisory-lock key that serialises every writer of the user's M3 entitlement (enable,
// disable and the expiry), for a test that must hold exactly that lock. The id is canonicalised (lower case)
// first, so the key does not depend on how the caller spelled the uuid.
func LockKey(userID string) int64 { return lockKey(strings.ToLower(userID), moduleM3) }

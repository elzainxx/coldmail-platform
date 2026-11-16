package money

import (
	"fmt"
	"testing"
)

const (
	lockUserA = "11111111-1111-4111-8111-111111111111"
	lockUserB = "22222222-2222-4222-8222-222222222222"
)

// fnv1a64 is an independent FNV-1a 64-bit hash written out from the published constants (offset basis 14695981039346656037, prime 1099511628211), so that
// lockKey is checked against the ALGORITHM and not against itself.
func fnv1a64(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func TestLockKey_IsFNV1a64OfUserBarModuleInsideThePlatformBand(t *testing.T) {
	// the reference hash itself is pinned by the one vector that needs no memory of a table: the hash of the empty string is the offset basis
	if got := fnv1a64(""); got != 14695981039346656037 {
		t.Fatalf("the reference FNV-1a of the empty string is %d, want the offset basis 14695981039346656037", got)
	}
	for _, c := range []struct{ user, module string }{{lockUserA, "m1"}, {lockUserA, "m2"}, {lockUserB, "m1"}, {"", ""}, {"x", "y"}} {
		want := 7_000_000_000 + int64(fnv1a64(c.user+"|"+c.module)%1_000_000_000)
		if got := lockKey(c.user, c.module); got != want {
			t.Errorf("lockKey(%q, %q) = %d, want 7e9 + FNV-1a-64(%q) mod 1e9 = %d", c.user, c.module, got, c.user+"|"+c.module, want)
		}
	}
}

func TestLockKey_AlwaysInThePlatformBand(t *testing.T) {
	// 5000 distinct pairs: every key lies in [7e9, 8e9), the band CLAUDE.md rule 9 reserves for the platform (M3 holds 8_xxx_xxx_xxx, M2 9_xxx_xxx_xxx)
	for i := 0; i < 5000; i++ {
		user := fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i*7919)
		for _, module := range []string{"m1", "m2"} {
			k := lockKey(user, module)
			if k < 7_000_000_000 || k >= 8_000_000_000 {
				t.Fatalf("lockKey(%q, %q) = %d, outside [7000000000, 8000000000)", user, module, k)
			}
		}
	}
	// the exported twin is the same function
	if LockKey(lockUserA, "m1") != lockKey(lockUserA, "m1") {
		t.Fatal("LockKey and lockKey disagree")
	}
}

func TestLockKey_IsStable(t *testing.T) {
	first := lockKey(lockUserA, "m1")
	for i := 0; i < 100; i++ {
		if got := lockKey(lockUserA, "m1"); got != first {
			t.Fatalf("lockKey changed between calls: %d then %d", first, got)
		}
	}
}

func TestLockKey_DifferentPairsDiffer(t *testing.T) {
	// the same user on the two metered modules, and two users on one module, must not share a lock: they would queue behind each other for no reason
	pairs := []struct{ user, module string }{{lockUserA, "m1"}, {lockUserA, "m2"}, {lockUserB, "m1"}, {lockUserB, "m2"}}
	seen := map[int64]string{}
	for _, p := range pairs {
		k := lockKey(p.user, p.module)
		if other, dup := seen[k]; dup {
			t.Fatalf("lockKey(%q, %q) = %d is also the key of %s", p.user, p.module, k, other)
		}
		seen[k] = p.user + "|" + p.module
	}
	// and the hash spreads: 5000 pairs give at least 4990 distinct keys (a constant or a tiny range would fail this)
	distinct := map[int64]bool{}
	for i := 0; i < 5000; i++ {
		distinct[lockKey(fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i*31), "m1")] = true
	}
	if len(distinct) < 4990 {
		t.Fatalf("5000 pairs gave only %d distinct keys: the hash does not spread", len(distinct))
	}
}

func TestLockKey_OrderOfThePairMatters(t *testing.T) {
	// ("a", "b") and ("b", "a") are different money; they must be different locks (the separator keeps "ab"+"c" from colliding with "a"+"bc" too)
	if lockKey("a", "b") == lockKey("b", "a") {
		t.Fatal("lockKey(a, b) == lockKey(b, a): the order of the pair does not matter")
	}
	if lockKey("ab", "c") == lockKey("a", "bc") {
		t.Fatal("lockKey(ab, c) == lockKey(a, bc): the separator does not separate")
	}
}

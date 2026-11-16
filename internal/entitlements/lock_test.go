package entitlements

import (
	"strings"
	"testing"
)

// The advisory-lock key is a number two packages (this one and internal/money) must compute identically, and a
// number the harness tests hold from a second connection to prove contention. Its vectors are pinned here,
// computed independently of this code (FNV-1a 64 over "<user>|<module>", mod 10^9, plus 7_000_000_000).
func TestLockKey_PinnedVectors(t *testing.T) {
	for _, tc := range []struct {
		user, module string
		want         int64
	}{
		{"11111111-1111-1111-1111-111111111111", "m3", 7_652_549_623},
		{"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "m3", 7_793_787_479},
		{"11111111-1111-1111-1111-111111111111", "m1", 7_675_806_045},
		{"", "", 7_299_834_427},
	} {
		if got := lockKey(tc.user, tc.module); got != tc.want {
			t.Errorf("lockKey(%q, %q) = %d, want %d", tc.user, tc.module, got, tc.want)
		}
	}
	// The module is part of the key: the same user's M1 and M3 locks are different locks (paired control: if the
	// module were ignored the two pinned vectors above could not differ).
	if lockKey("11111111-1111-1111-1111-111111111111", "m1") == lockKey("11111111-1111-1111-1111-111111111111", "m3") {
		t.Error("the module does not enter the key")
	}
}

func TestLockKey_StaysInThePlatformBand(t *testing.T) {
	// CLAUDE.md rule 9: the platform holds 7_xxx_xxx_xxx and no other band (M3 holds 8_, M2 holds 9_).
	for i := 0; i < 5000; i++ {
		user := strings.Repeat("a", i%7) + string(rune('a'+i%26)) + "-" + strings.Repeat("7", i%31)
		k := lockKey(user, moduleM3)
		if k < 7_000_000_000 || k > 7_999_999_999 {
			t.Fatalf("lockKey(%q) = %d is outside the 7_xxx_xxx_xxx band", user, k)
		}
	}
	// Paired control: the band check can fail, a key outside it is seen.
	if k := int64(8_000_000_000); k >= 7_000_000_000 && k <= 7_999_999_999 {
		t.Fatal("control failed: 8_000_000_000 was read as inside the platform band")
	}
}

func TestLockKey_ExportedKeyIsTheCanonicalM3Key(t *testing.T) {
	const upper = "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"
	if got, want := LockKey(upper), int64(7_793_787_479); got != want {
		t.Errorf("LockKey of an upper-case uuid = %d, want the lower-case one's %d", got, want)
	}
	if LockKey("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa") != lockKey("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "m3") {
		t.Error("LockKey is not lockKey(user, \"m3\")")
	}
	if LockKey("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa") == LockKey("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb") {
		t.Error("two users share a key in this sample: the key does not depend on the user")
	}
}

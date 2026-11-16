package entitlements

import (
	"testing"
	"time"
)

func TestDefaultBackoff_Table(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{
		{-5, 1 * time.Second}, // a negative count is treated as none
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
		{6, 60 * time.Second}, // 64 s capped
		{7, 60 * time.Second},
		{20, 60 * time.Second},
		{62, 60 * time.Second},
		{63, 60 * time.Second},
		{64, 60 * time.Second}, // where a naive shift wraps to zero
		{65, 60 * time.Second},
		{1 << 30, 60 * time.Second},
	} {
		if got := DefaultBackoff(tc.attempts); got != tc.want {
			t.Errorf("DefaultBackoff(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}

func TestDefaultBackoff_NeverShrinksAndNeverExceedsTheCap(t *testing.T) {
	prev := time.Duration(0)
	for n := 0; n < 200; n++ {
		d := DefaultBackoff(n)
		if d < prev {
			t.Fatalf("DefaultBackoff(%d) = %s is shorter than DefaultBackoff(%d) = %s: a failing user would be retried FASTER", n, d, n-1, prev)
		}
		if d <= 0 || d > time.Minute {
			t.Fatalf("DefaultBackoff(%d) = %s is outside (0, 60s]", n, d)
		}
		prev = d
	}
	// Paired control: the arithmetic a careless version would use DOES wrap, so the clamp in DefaultBackoff is
	// what keeps the schedule sane at a large count (the table above would not catch a missing clamp at 1<<30
	// otherwise; here the wrap is shown to exist).
	naive := func(n uint) time.Duration { return time.Duration(1<<n) * time.Second }
	if naive(64) != 0 {
		t.Fatalf("control failed: a plain shift by 64 gives %s, not the zero that makes a clamp necessary", naive(64))
	}
}

func TestDefaultBackoff_IsDeterministic(t *testing.T) {
	// No jitter: the same count gives the same delay every time, so a test can pin the schedule.
	for n := 0; n < 10; n++ {
		if DefaultBackoff(n) != DefaultBackoff(n) {
			t.Fatalf("DefaultBackoff(%d) is not a pure function of its argument", n)
		}
	}
}

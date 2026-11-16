package money

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// Limits. Every one is checked BEFORE the database is asked, so a malformed request costs no connection and writes nothing; the database's own CHECKs
// stay as the second wall.
const (
	// MaxAmount is the largest single grant or adjustment: 2^53, the largest integer that a float64 (a JSON number in a browser) holds exactly.
	MaxAmount int64 = 1 << 53
	// MaxReasonBytes bounds an admin's reason, after trimming.
	MaxReasonBytes = 1000
	// MaxKeyBytes bounds a caller's idempotency key.
	MaxKeyBytes = 200
	// MaxItemRefBytes bounds a settle item's reference (the database refuses more than 200 bytes too).
	MaxItemRefBytes = 200
	// MaxUnits is the largest reserve in units (ledger.units is an integer column).
	MaxUnits = math.MaxInt32
	// MaxSettleItems bounds one Settle call: a caller pages a bigger job, and the advisory lock is held for the whole call.
	MaxSettleItems = 10000
	// SystemUser is module 2's shared-pool owner. It holds nothing (platform_0004), so no request may name it.
	SystemUser = "00000000-0000-0000-0000-000000000000"
)

// reservedKeyPrefixes are the shapes of the keys the store derives itself ('reserve:<job>', 'charge:<job>:<item>', 'release:<reserve>'). They share one
// key space with a caller's grant and adjustment keys (UNIQUE (user, module, key)), so a caller key in that shape could make a later reserve, charge or
// release silently "already done". A caller may not use them.
var reservedKeyPrefixes = []string{"reserve:", "charge:", "release:"}

// isCanonicalUUID is true for exactly the form Postgres prints: 36 characters, lower-case hex, hyphens at 8, 13, 18 and 23. A different spelling of the same
// uuid would hash to a different advisory lock, so only this one is accepted.
func isCanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

func checkUser(field, v string) error {
	if !isCanonicalUUID(v) {
		return invalid(field, "must be a canonical lower-case uuid, got %q", clip(v))
	}
	if v == SystemUser {
		return invalid(field, "the system user holds no credits and cannot be named")
	}
	return nil
}

func checkUUID(field, v string) error {
	if !isCanonicalUUID(v) {
		return invalid(field, "must be a canonical lower-case uuid, got %q", clip(v))
	}
	return nil
}

// checkMetered accepts m1 and m2. m3 is a real module that holds no credits (ErrNotMetered); anything else is not a module of this store at all.
func checkMetered(module string) error {
	switch module {
	case "m1", "m2":
		return nil
	case "m3":
		return fmt.Errorf("%w: module m3", ErrNotMetered)
	}
	return invalid("module", "must be m1 or m2, got %q", clip(module))
}

func checkAmount(v int64) error {
	if v < 1 || v > MaxAmount {
		return invalid("amount", "must be between 1 and %d credits, got %d", MaxAmount, v)
	}
	return nil
}

// cleanReason trims the reason (Go's notion of white space) and refuses a blank, an over-long, an invalid-UTF-8 or a NUL-carrying one. The trimmed text is what is stored.
func cleanReason(v string) (string, error) {
	if !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
		return "", invalid("reason", "must be valid UTF-8 text without NUL")
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", invalid("reason", "must not be blank")
	}
	if len(v) > MaxReasonBytes {
		return "", invalid("reason", "is %d bytes, the limit is %d", len(v), MaxReasonBytes)
	}
	return v, nil
}

// checkKey validates an idempotency key. It is NOT trimmed: the key is an identity, and trimming it would change which request it names.
func checkKey(v string) error {
	if !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
		return invalid("key", "must be valid UTF-8 text without NUL")
	}
	if strings.TrimSpace(v) == "" {
		return invalid("key", "must not be blank")
	}
	if len(v) > MaxKeyBytes {
		return invalid("key", "is %d bytes, the limit is %d", len(v), MaxKeyBytes)
	}
	for _, p := range reservedKeyPrefixes {
		if strings.HasPrefix(v, p) {
			return invalid("key", "must not start with %q: that shape is reserved for the keys the store derives", p)
		}
	}
	return nil
}

// entryReq is a validated grant or adjustment.
type entryReq struct {
	kind    string // "grant" or "adjustment"
	userID  string
	module  string
	amount  int64 // credits, always > 0; an adjustment REMOVES them
	adminID string
	reason  string // trimmed
	key     string
}

func validateEntry(kind, userID, module string, amount int64, adminID, reason, key string) (entryReq, error) {
	r := entryReq{kind: kind, userID: userID, module: module, amount: amount, adminID: adminID, key: key}
	if err := checkUser("user_id", userID); err != nil {
		return r, err
	}
	if err := checkMetered(module); err != nil {
		return r, err
	}
	if err := checkAmount(amount); err != nil {
		return r, err
	}
	if err := checkUser("admin_id", adminID); err != nil {
		return r, err
	}
	clean, err := cleanReason(reason)
	if err != nil {
		return r, err
	}
	r.reason = clean
	if err := checkKey(key); err != nil {
		return r, err
	}
	return r, nil
}

func validateReserve(req ReserveReq) error {
	if err := checkUUID("job_id", req.JobID); err != nil {
		return err
	}
	if req.Units < 1 || req.Units > MaxUnits {
		return invalid("units", "must be between 1 and %d, got %d", MaxUnits, req.Units)
	}
	return nil
}

func validateRelease(jobID string) error { return checkUUID("job_id", jobID) }

func validateSettle(req SettleReq) error {
	if err := checkUUID("job_id", req.JobID); err != nil {
		return err
	}
	if len(req.Items) > MaxSettleItems {
		return invalid("items", "%d items in one call, the limit is %d: settle a bigger job in pages", len(req.Items), MaxSettleItems)
	}
	seen := make(map[string]int, len(req.Items))
	for i, it := range req.Items {
		if !utf8.ValidString(it.ItemRef) || strings.ContainsRune(it.ItemRef, 0) {
			return fmt.Errorf("%w: item %d: the item_ref must be valid UTF-8 text without NUL", ErrBadItem, i)
		}
		if strings.TrimSpace(it.ItemRef) == "" {
			return fmt.Errorf("%w: item %d: the item_ref must not be blank", ErrBadItem, i)
		}
		if len(it.ItemRef) > MaxItemRefBytes {
			return fmt.Errorf("%w: item %d: the item_ref is %d bytes, the limit is %d", ErrBadItem, i, len(it.ItemRef), MaxItemRefBytes)
		}
		if j, dup := seen[it.ItemRef]; dup {
			return fmt.Errorf("%w: item %d repeats the item_ref of item %d (%q)", ErrBadItem, i, j, clip(it.ItemRef))
		}
		seen[it.ItemRef] = i
	}
	return nil
}

func validateBalance(userID, module string) error {
	if err := checkUser("user_id", userID); err != nil {
		return err
	}
	return checkMetered(module)
}

// clip shortens a value for an error message, so that a hostile 1 MB "uuid" cannot fill a log line.
func clip(s string) string {
	const limit = 64
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

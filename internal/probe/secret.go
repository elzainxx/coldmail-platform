package probe

import (
	"fmt"
	"io"
	"log/slog"
)

// redacted is what a Secret prints as, whatever the verb or sink.
const redacted = "[redacted]"

// Secret is a credential that cannot be printed by accident: every way the standard library renders a
// value (fmt with any verb, encoding/json, slog) yields "[redacted]". The only way to the real value is
// Reveal, which is greppable at the one place that needs it (the Authorization header) and at the
// Recorder, which scrubs every registered secret out of every line it writes.
type Secret string

// Reveal returns the underlying value. Call it only where the value is consumed.
func (s Secret) Reveal() string { return string(s) }

// Format renders redacted for EVERY verb (without it a verb fmt does not route through String, %d for
// instance, prints the underlying string).
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

func (Secret) String() string   { return redacted }
func (Secret) GoString() string { return redacted }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText covers encoding/json and any other text sink.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

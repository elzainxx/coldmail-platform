package config

import (
	"fmt"
	"io"
	"log/slog"
)

// redacted is what a Secret prints as, whatever the verb or sink.
const redacted = "[redacted]"

// Secret is a string that cannot be printed by accident. Every way the standard library renders a value
// (fmt with any verb, encoding/json, encoding/text, slog) yields "[redacted]"; the only way to the real
// value is the explicit Reveal call, which is therefore greppable at every site that needs it.
//
// This is defence in depth beneath Config's own String/GoString/LogValue: it protects the FIELD, so
// `slog.Info("x", "url", cfg.DatabaseURL)` and `fmt.Sprintf("%d", cfg.DatabaseURL)` are as safe as
// printing the whole Config.
//
// TWO HOLES THAT NO METHOD CAN CLOSE, both measured at Go 1.26. (1) fmt handles %p BEFORE it consults any
// method, and for a non-pointer it prints the raw value as part of its bad-verb text, so
// fmt.Sprintf("%p", secret) reveals the string; `go vet` rejects that call statically (%p of a non-pointer),
// which is why vet is a CI gate. (2) A reflection-based dumper that reads an UNEXPORTED field bypasses
// method dispatch entirely; nothing in this repository does that, and the rule is: never dump a Config
// with one.
type Secret string

// Reveal returns the underlying value. Call it only where the value is consumed (a driver call), never
// where it could be formatted.
func (s Secret) Reveal() string { return string(s) }

// Format renders redacted for EVERY verb. Without it, a verb fmt does not route through String (%d, for
// instance) prints the underlying string.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

func (Secret) String() string   { return redacted }
func (Secret) GoString() string { return redacted }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText covers encoding/json (a TextMarshaler is encoded as a JSON string) and any other text sink.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

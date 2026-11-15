// Package logging builds the platform's structured logger and carries the request id through a
// context, so that EVERY log line a request produces names the request without each call site having to
// remember to.
package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
)

// RequestIDKey is the attribute name under which the request id appears in every log line.
const RequestIDKey = "request_id"

// levelNames is the vocabulary of PLATFORM_LOG_LEVEL, in ascending severity.
var levelNames = []string{"debug", "info", "warn", "error"}

// LevelNames returns the accepted level names (a copy).
func LevelNames() []string { return slices.Clone(levelNames) }

// ParseLevel maps a level name (case-insensitive, surrounding space ignored) to a slog level. The error
// never echoes the input.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level (want one of: %s)", strings.Join(levelNames, ", "))
}

// New returns a JSON logger writing to w at the given level. Timestamps are UTC. Every call that passes a
// context carrying a request id (see WithRequestID) gets a request_id attribute on its line.
func New(level string, w io.Writer) (*slog.Logger, error) {
	lv, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	if w == nil {
		return nil, errors.New("logging: writer is nil")
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
				a.Value = slog.TimeValue(a.Value.Time().UTC())
			}
			return a
		},
	})
	return slog.New(ctxHandler{h}), nil
}

type ctxKey struct{}

// WithRequestID returns a context carrying the request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// RequestID returns the request id in ctx, or "" when there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// ctxHandler adds the request id of the call's context to every record. It is what makes "every log line
// the request produces carries the id" a property of the logger rather than a convention at each call
// site. A call that passes no context (logger.Info, not logger.InfoContext) cannot carry one.
type ctxHandler struct{ slog.Handler }

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r = r.Clone() // never append into a record whose backing array the caller may share
		r.AddAttrs(slog.String(RequestIDKey, id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h ctxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ctxHandler{h.Handler.WithAttrs(attrs)}
}

func (h ctxHandler) WithGroup(name string) slog.Handler {
	return ctxHandler{h.Handler.WithGroup(name)}
}

// Contextual returns l unchanged when it already attaches request ids, and a wrapped copy when it does
// not (a logger built with slog.New directly, say). Idempotent, so a line never carries the key twice.
func Contextual(l *slog.Logger) *slog.Logger {
	if _, ok := l.Handler().(ctxHandler); ok {
		return l
	}
	return slog.New(ctxHandler{l.Handler()})
}

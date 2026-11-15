package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// lines decodes every JSON log line written to buf. It fails on a line that is not JSON, and the caller
// asserts a non-zero count: a logger that wrote nothing must not pass an "every line has X" check.
func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", raw, err)
		}
		out = append(out, m)
	}
	return out
}

func TestNew_JSONLevelAndUTCTimestamps(t *testing.T) {
	var buf bytes.Buffer
	log, err := New("info", &buf)
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("dropped at info")
	log.Info("kept", "k", "v")

	got := lines(t, &buf)
	if len(got) != 1 {
		t.Fatalf("an info logger wrote %d lines for one debug and one info call, want 1: %s", len(got), buf.String())
	}
	l := got[0]
	if l["msg"] != "kept" || l["level"] != "INFO" || l["k"] != "v" {
		t.Fatalf("unexpected line: %v", l)
	}
	ts, ok := l["time"].(string)
	if !ok || !strings.HasSuffix(ts, "Z") {
		t.Fatalf("time %q is not UTC (no Z suffix)", l["time"])
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Fatalf("time %q is not RFC3339: %v", ts, err)
	}

	// Paired control: the same call IS written at debug level, so the line above was dropped by the
	// level filter and not by a writer that swallows everything.
	buf.Reset()
	dbg, err := New("debug", &buf)
	if err != nil {
		t.Fatal(err)
	}
	dbg.Debug("now kept")
	if got := lines(t, &buf); len(got) != 1 || got[0]["msg"] != "now kept" {
		t.Fatalf("a debug logger dropped a debug line: %s", buf.String())
	}
}

func TestNew_RejectsBadLevelAndNilWriter(t *testing.T) {
	var buf bytes.Buffer
	_, err := New("verbose-CANARY", &buf)
	if err == nil {
		t.Fatal("an unknown level was accepted")
	}
	if strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("the error echoes its input: %v", err)
	}
	for _, n := range LevelNames() {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("the error does not list the valid level %q: %v", n, err)
		}
	}
	if _, err := New("info", nil); err == nil {
		t.Fatal("a nil writer was accepted")
	}

	// Paired control: every documented name is accepted, in any case.
	for _, n := range LevelNames() {
		for _, spelled := range []string{n, strings.ToUpper(n), " " + n + " "} {
			if _, err := New(spelled, &buf); err != nil {
				t.Errorf("New(%q) = %v, want ok", spelled, err)
			}
		}
	}
}

func TestParseLevel_VocabularyIsOrderedAndCopied(t *testing.T) {
	names := LevelNames()
	if len(names) != 4 {
		t.Fatalf("LevelNames() = %v, want four levels", names)
	}
	prev := slog.Level(-1 << 30)
	for _, n := range names {
		lv, err := ParseLevel(n)
		if err != nil {
			t.Fatalf("ParseLevel(%q): %v", n, err)
		}
		if lv <= prev {
			t.Fatalf("level %q (%d) is not more severe than the one before it (%d)", n, lv, prev)
		}
		prev = lv
	}
	names[0] = "mutated"
	if LevelNames()[0] != "debug" {
		t.Fatal("LevelNames returned the package's own slice, not a copy")
	}
	if _, err := ParseLevel("warning"); err == nil {
		t.Fatal(`"warning" is not in the vocabulary and must be rejected`)
	}
}

func TestRequestID_ContextRoundTrip(t *testing.T) {
	if got := RequestID(context.Background()); got != "" {
		t.Fatalf("an empty context yielded request id %q", got)
	}
	ctx := WithRequestID(context.Background(), "req_a")
	if got := RequestID(ctx); got != "req_a" {
		t.Fatalf("RequestID = %q, want req_a", got)
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	if got := RequestID(child); got != "req_a" {
		t.Fatalf("a derived context lost the id: %q", got)
	}
	if got := RequestID(WithRequestID(ctx, "req_b")); got != "req_b" {
		t.Fatalf("an inner id did not override: %q", got)
	}
	if got := RequestID(ctx); got != "req_a" {
		t.Fatalf("overriding in a child changed the parent: %q", got)
	}
}

func TestLogger_RequestIDOnEveryLineThatCarriesAContext(t *testing.T) {
	var buf bytes.Buffer
	log, err := New("debug", &buf)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithRequestID(context.Background(), "req_0123")

	log.InfoContext(ctx, "with id")
	log.With("scoped", 1).WarnContext(ctx, "with id after With")
	log.ErrorContext(ctx, "with id and attrs", "a", 1, "b", "two")
	log.Info("without a context")
	log.InfoContext(context.Background(), "empty context")

	got := lines(t, &buf)
	if len(got) != 5 {
		t.Fatalf("got %d lines, want 5: %s", len(got), buf.String())
	}
	for i, l := range got[:3] {
		if l[RequestIDKey] != "req_0123" {
			t.Errorf("line %d (%v) lacks the request id", i, l["msg"])
		}
	}
	if got[1]["scoped"] != float64(1) {
		t.Errorf("With() attributes were lost by the context handler: %v", got[1])
	}
	// Paired control: the id is not invented where no context carried one.
	for i, l := range got[3:] {
		if _, has := l[RequestIDKey]; has {
			t.Errorf("line %d (%v) has a request id although its context carried none", i+3, l["msg"])
		}
	}
}

func TestContextual_WrapsPlainLoggersOnceAndOnlyOnce(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req_9")

	// A logger built without this package carries no id...
	var plain bytes.Buffer
	raw := slog.New(slog.NewJSONHandler(&plain, nil))
	raw.InfoContext(ctx, "raw")
	if got := lines(t, &plain); len(got) != 1 {
		t.Fatalf("raw logger wrote %d lines", len(got))
	} else if _, has := got[0][RequestIDKey]; has {
		t.Fatal("control failed: a plain slog logger already attaches request ids, so Contextual proves nothing")
	}

	// ...and Contextual gives it one.
	plain.Reset()
	Contextual(raw).InfoContext(ctx, "wrapped")
	got := lines(t, &plain)
	if len(got) != 1 || got[0][RequestIDKey] != "req_9" {
		t.Fatalf("Contextual did not attach the id: %s", plain.String())
	}

	// Idempotent: wrapping twice (or wrapping the output of New) does not duplicate the key.
	var buf bytes.Buffer
	l, err := New("info", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if Contextual(l) != l {
		t.Fatal("Contextual re-wrapped a logger that already attaches ids")
	}
	Contextual(Contextual(l.With("x", 1))).InfoContext(ctx, "once")
	if n := strings.Count(buf.String(), `"`+RequestIDKey+`"`); n != 1 {
		t.Fatalf("the line carries %d request_id keys, want exactly 1: %s", n, buf.String())
	}
}

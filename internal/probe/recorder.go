package probe

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Kind says what a check is for: a Positive check asserts that a good call is answered well, a Negative
// check asserts that a call with no or a wrong credential is refused (and so proves the positive ones
// can fail).
type Kind int

const (
	Positive Kind = iota
	Negative
)

func (k Kind) String() string {
	if k == Negative {
		return "negative"
	}
	return "positive"
}

// Status is the verdict of one check.
type Status int

const (
	StatusPass Status = iota + 1
	StatusFail
	StatusNotApplicable
)

// Result is one recorded check.
type Result struct {
	ID     string
	Kind   Kind
	Status Status
	Reason string // the failure reason, or why the check does not apply; empty for a pass
}

// maxLineRunes caps one output line, so a reason can never become a body dump.
const maxLineRunes = 400

// Recorder writes the check lines to out and the request lines to log the moment they happen, keeps the
// results, and scrubs every registered secret out of every line before it is written.
type Recorder struct {
	out     io.Writer
	log     io.Writer
	secrets []string
	results []Result
}

// NewRecorder returns a Recorder. Empty secrets are ignored (replacing "" would garble every line).
func NewRecorder(out, log io.Writer, secrets ...Secret) *Recorder {
	r := &Recorder{out: out, log: log}
	for _, s := range secrets {
		if s != "" {
			r.secrets = append(r.secrets, s.Reveal())
		}
	}
	return r
}

// Pass records a passing check.
func (r *Recorder) Pass(id string, kind Kind) {
	r.results = append(r.results, Result{ID: id, Kind: kind, Status: StatusPass})
	r.emit(r.out, "CHECK "+id+" PASS")
}

// Fail records a failing check. The reason says what was expected and what was got, and carries no body.
func (r *Recorder) Fail(id string, kind Kind, reason string) {
	r.results = append(r.results, Result{ID: id, Kind: kind, Status: StatusFail, Reason: reason})
	r.emit(r.out, "CHECK "+id+" FAIL "+reason)
}

// NotApplicable records a check that does not apply. It is not a pass and is counted apart.
func (r *Recorder) NotApplicable(id, why string) {
	r.results = append(r.results, Result{ID: id, Status: StatusNotApplicable, Reason: why})
	r.emit(r.out, "NOT-APPLICABLE "+id+" "+why)
}

// request prints the line for one outgoing request. auth is a label, never a credential.
func (r *Recorder) request(method, target, auth string) {
	r.emit(r.log, "REQUEST "+method+" "+target+" auth="+auth)
}

// Results returns every result recorded so far, in order.
func (r *Recorder) Results() []Result { return append([]Result(nil), r.results...) }

// Counts returns the checks executed (passes plus failures; a not-applicable line is not a check), the
// passes, the failures and the not-applicable lines.
func (r *Recorder) Counts() (checks, pass, fail, notApplicable int) {
	for _, res := range r.results {
		switch res.Status {
		case StatusPass:
			pass++
		case StatusFail:
			fail++
		case StatusNotApplicable:
			notApplicable++
		}
	}
	return pass + fail, pass, fail, notApplicable
}

// emit scrubs, flattens to one line, caps the length and writes. The scrub comes first so that the cap
// cannot leave a fragment of a secret behind.
func (r *Recorder) emit(w io.Writer, line string) {
	for _, s := range r.secrets {
		line = strings.ReplaceAll(line, s, redacted)
	}
	line = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return ' '
		}
		return c
	}, line)
	if runes := []rune(line); len(runes) > maxLineRunes {
		line = string(runes[:maxLineRunes-3]) + "..."
	}
	_, _ = fmt.Fprintln(w, line)
}

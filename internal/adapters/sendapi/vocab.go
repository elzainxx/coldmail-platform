package sendapi

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// The describe route is not tenant scoped (one token serves every tenant), so these are package functions over the module client and a Profile, not methods of a tenant's Client.

func bindError(mc *modclient.Client, p Profile, op string) error {
	bad := func(msg string) error {
		return &modclient.Error{Kind: modclient.Invalid, Module: p.Module, Route: p.route(op), Message: msg}
	}
	switch {
	case mc == nil:
		return bad("there is no module client")
	case mc.Module() != p.Module:
		return bad("the module client was built for another module than the profile")
	}
	if err := p.validate(); err != nil {
		return bad(err.Error())
	}
	return nil
}

// Describe reads the module's self-description (GET /v1/describe) and checks that it is the module the profile is for and the API version the adapter was written against: a describe of module
// 3 answered at module 2's address is a wiring mistake only the adapter can name (Contract error, Key "module"; a version other than v1 is Key "api_version"). ON ANY ERROR THE RESULT IS NIL.
func Describe(ctx context.Context, mc *modclient.Client, p Profile) (*modclient.Describe, error) {
	if err := bindError(mc, p, "describe"); err != nil {
		return nil, err
	}
	d, err := mc.Describe(ctx)
	if err != nil {
		return nil, err
	}
	contract := func(key, msg string) error {
		return &modclient.Error{Kind: modclient.Contract, Module: p.Module, Route: p.route("describe"), Key: key, Message: msg, RequestID: d.RequestID, Sent: true}
	}
	switch {
	case d.Module != p.DescribeModule:
		return nil, contract("module", "the describe names another module than the one this client is wired to (the base URL or the token of the wrong module)")
	case d.APIVersion != apiVersion:
		return nil, contract("api_version", "the module serves another API version than the one the adapter was written against")
	}
	return d, nil
}

// Vocabularies is Describe's vocabularies: the value sets the module advertises, by describe's names. ON ANY ERROR THE RESULT IS NIL.
func Vocabularies(ctx context.Context, mc *modclient.Client, p Profile) (modclient.Vocabularies, error) {
	d, err := Describe(ctx, mc, p)
	if err != nil {
		return nil, err
	}
	return d.Vocabularies, nil
}

// VocabReport is what a describe says against what the profile handles.
type VocabReport struct {
	// Diffs has one entry per vocabulary the profile handles (job_state, error_class, and item_reason when the module has reasons), in that order.
	Diffs []modclient.VocabDiff
	// CodesMissing are error codes the adapter maps that the module no longer lists; CodesExtra are codes it lists that the adapter does not know (additive: the status decides the kind).
	CodesMissing []string
	CodesExtra   []string
}

// Breaking is true when the module no longer offers what the adapter relies on or offers a state the adapter does not handle: a vocabulary that is gone, a value of it that is missing,
// an error code that is missing, or an EXTRA value in a strict vocabulary (job_state: a new state is a contract violation at run time, so advertising one is the same break found early).
func (r VocabReport) Breaking() bool { return r.firstBreak() != "" }

// Additive is true when the module advertises values the adapter carries without handling (extra error classes, reasons or error codes): nothing breaks, and a person should look.
func (r VocabReport) Additive() bool {
	if len(r.CodesExtra) > 0 {
		return true
	}
	for _, d := range r.Diffs {
		if d.Advertised && len(d.Extra) > 0 && !strictVocabularies[d.Name] {
			return true
		}
	}
	return false
}

// firstBreak names the first break ("" when none): "vocabularies.<name>" or "error_codes".
func (r VocabReport) firstBreak() string {
	for _, d := range r.Diffs {
		if d.Breaking() || (strictVocabularies[d.Name] && len(d.Extra) > 0) {
			return "vocabularies." + d.Name
		}
	}
	if len(r.CodesMissing) > 0 {
		return vocabErrorCodes
	}
	return ""
}

// CompareVocabularies is the pure comparison: the describe a module answered against the sets the profile's adapter handles.
func CompareVocabularies(d *modclient.Describe, p Profile) VocabReport {
	var r VocabReport
	want := p.Vocabularies()
	for _, name := range []string{vocabJobState, vocabErrorClass, vocabItemReason} {
		expected, ok := want[name]
		if !ok {
			continue
		}
		r.Diffs = append(r.Diffs, d.Vocabularies.Diff(name, expected))
	}
	have := map[string]bool{}
	for _, ec := range d.ErrorCodes {
		have[ec.Code] = true
	}
	for _, code := range want[vocabErrorCodes] {
		if !have[code] {
			r.CodesMissing = append(r.CodesMissing, code)
		}
	}
	for code := range have {
		if !slices.Contains(want[vocabErrorCodes], code) {
			r.CodesExtra = append(r.CodesExtra, code)
		}
	}
	sort.Strings(r.CodesMissing)
	sort.Strings(r.CodesExtra)
	return r
}

// CheckVocabularies reads the module's describe and compares it with the profile. A break (see VocabReport.Breaking) is a Contract error naming the vocabulary in Key and, in the message, the
// values the ADAPTER expects and the module no longer lists (never a value the module sent); the report is then not returned. An additive difference is not an error: it is in the
// returned report. ON ANY ERROR THE REPORT IS THE ZERO VALUE.
func CheckVocabularies(ctx context.Context, mc *modclient.Client, p Profile) (VocabReport, error) {
	d, err := Describe(ctx, mc, p)
	if err != nil {
		return VocabReport{}, err
	}
	r := CompareVocabularies(d, p)
	key := r.firstBreak()
	if key == "" {
		return r, nil
	}
	msg := "the module's describe no longer matches what the adapter handles"
	for _, df := range r.Diffs {
		if "vocabularies."+df.Name != key {
			continue
		}
		switch {
		case !df.Advertised:
			msg = "the module no longer advertises this vocabulary"
		case len(df.Missing) > 0:
			msg = fmt.Sprintf("the module no longer lists the value(s) the adapter handles: %s", strings.Join(df.Missing, ", "))
		default:
			msg = fmt.Sprintf("the module lists %d value(s) of this closed set that the adapter does not handle (a new state is never mapped by guesswork)", len(df.Extra))
		}
	}
	if key == vocabErrorCodes {
		msg = fmt.Sprintf("the module no longer lists the error code(s) the adapter maps: %s", strings.Join(r.CodesMissing, ", "))
	}
	return VocabReport{}, &modclient.Error{Kind: modclient.Contract, Module: p.Module, Route: p.route("describe"), Key: key, Message: msg, RequestID: d.RequestID, Sent: true}
}

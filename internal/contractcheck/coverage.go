package contractcheck

import (
	"fmt"
	"io"

	"github.com/coldmail-platform/platform/internal/modclient"
)

// Exercised names one module response by the harness test that called it live (or, in a contract, by the contract that declares it).
type Exercised struct {
	Module modclient.Module
	Route  string
	Case   string
}

func (e Exercised) String() string {
	c := ""
	if e.Case != "" {
		c = " case=" + e.Case
	}
	return fmt.Sprintf("%s %s%s", e.Module, e.Route, c)
}

// BlindSpots is the sentence every coverage report ends with: what the drift gate cannot see, whatever its counts say.
const BlindSpots = "NOT SEEN BY THIS GATE: response bodies for inputs the harness never sends; fields the module adds in states the harness never reaches; anything the module does only in production."

// CoverageReport says how much of the declared contracts the harness actually compared with a live call.
type CoverageReport struct {
	// Contracts is the number of distinct contracts (module, route, case).
	Contracts int
	// Covered is how many of them had at least one live call.
	Covered int
	// Calls is the number of distinct live calls made.
	Calls int
	// Never are the contracts no live call exercised: a fixture that was never compared.
	Never []Exercised
	// Undeclared are the live calls no contract declares: a dependency on the module that nothing pins.
	Undeclared []Exercised
}

// Coverage matches the declared contracts against what the harness called.
func Coverage(contracts []modclient.RouteContract, exercised []Exercised) CoverageReport {
	var r CoverageReport
	declared := map[Exercised]bool{}
	var order []Exercised
	for _, rc := range contracts {
		k := Exercised{rc.Module, rc.Route, rc.Case}
		if !declared[k] {
			declared[k] = true
			order = append(order, k)
		}
	}
	called := map[Exercised]bool{}
	var calls []Exercised
	for _, e := range exercised {
		if !called[e] {
			called[e] = true
			calls = append(calls, e)
		}
	}
	r.Contracts, r.Calls = len(order), len(calls)
	for _, k := range order {
		if called[k] {
			r.Covered++
		} else {
			r.Never = append(r.Never, k)
		}
	}
	for _, e := range calls {
		if !declared[e] {
			r.Undeclared = append(r.Undeclared, e)
		}
	}
	return r
}

// OK is true only when there is at least one contract, every contract was exercised and no live call is undeclared.
func (r CoverageReport) OK() bool {
	return r.Contracts > 0 && len(r.Never) == 0 && len(r.Undeclared) == 0
}

// Print writes the counts, each contract that was not compared and each call that no contract declares, and the fixed sentence about what the gate cannot see.
func (r CoverageReport) Print(w io.Writer) {
	if r.Contracts == 0 {
		fmt.Fprintln(w, "contract drift gate: found 0 contracts: the instrument matched nothing")
	} else {
		fmt.Fprintf(w, "contract drift gate: %d of %d contracts compared with a live response; %d never exercised; %d live calls with no contract (%d distinct live calls)\n",
			r.Covered, r.Contracts, len(r.Never), len(r.Undeclared), r.Calls)
	}
	for _, e := range r.Never {
		fmt.Fprintf(w, "NOT COMPARED: %s (declared, never called live)\n", e)
	}
	for _, e := range r.Undeclared {
		fmt.Fprintf(w, "UNDECLARED: %s (called live, no contract)\n", e)
	}
	fmt.Fprintln(w, BlindSpots)
}

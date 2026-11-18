//go:build harness && m1node

package harness

import (
	"testing"

	m1ad "github.com/coldmail-platform/platform/internal/adapters/m1"
	"github.com/coldmail-platform/platform/internal/contractcheck"
)

// Phase 3, chunk B6: the contract drift gate for module 1. Every response the REAL module 1 gave to an adapter built in this binary (the end-to-end tests' tenants and the node
// tests' live client) is compared with the recorded fixture of the contract it belongs to (internal/adapters/m1/testdata/contract): a key that went missing or appeared, a type
// that changed, a vocabulary value the adapter does not handle, or a status that changed fails the test naming the route, the case and the path. A response no contract declares
// fails (a dependency nobody wrote down); a contract no response was matched to fails unless it is listed below with the reason the harness cannot elicit it. The Session prints
// what it does not cover. This file is named zz_ so that its test is the LAST of the binary: Go runs a package's tests in the order of the files' names.
//
// m1Drift is created at package initialisation, before any test builds an adapter; m1bObserve (m1_bulk_settle_test.go) is the hook the untagged helpers read.
var m1Drift = contractcheck.NewSession(m1ad.Contracts(), m1ad.ContractFixtures(), "testdata/contract", m1DriftNotElicited)

// m1DriftNotElicited: module|route|case -> why the harness cannot make module 1 give that answer. Every entry needs a reason; an entry for a contract that WAS exercised is red
// (stale). EMPTY until the first runner run says which contracts the end-to-end tests do not reach.
var m1DriftNotElicited = map[string]string{}

func init() { m1bObserve = m1Drift.Observer() }

// TestM1Node_ZContractDrift is the gate itself (the last test of the binary).
func TestM1Node_ZContractDrift(t *testing.T) { m1Drift.Finish(t) }

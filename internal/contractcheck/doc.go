// Package contractcheck is the comparison behind the contract drift gate (Phase 3, B6). Every adapter lists the module responses it relies on as modclient.RouteContract
// values: the route, the HTTP status, the Shape the adapter decodes with, the closed vocabularies it maps from, and the name of a recorded response body under its
// testdata/contract directory. The adapters' unit tests decode the recorded bodies offline. The drift gate, which runs in the harness against the REAL module, makes each call
// and hands what came back to this package, which compares it with the recorded body. The package is plain Go: no network, no database, nothing here calls a module.
//
// What is in it:
//
//   - Skeleton: the structure of a body (keys, JSON types, arrays folded into one element, maps ignoring their keys where the Shape says MapOf).
//   - Compare: a live response against a fixture; the differences are StatusChanged, KeyMissing, KeyAdded, TypeChanged, VocabNew and ShapeViolation (and ContractInvalid when
//     the comparison itself cannot be made). A difference names the module, route, case and path and prints no value except a vocabulary value.
//   - CheckFixtures: the no-rot check (every contract has a usable fixture, every fixture has a contract, nothing secret-shaped is committed).
//   - Normalise, Record, ParseRecorded and WriteRecorded: record mode. The harness prints each live response as a delimited, normalised block in its log; the lead feeds the
//     downloaded log to cmd/contract-fixtures, which writes the files to commit.
//   - Coverage, AssertLive and Recorder: what the harness calls, and the report of how much of the contracts it did not exercise.
//
// THE PATH GRAMMAR. A path names a position in a body: an object key is its name joined to its parent with a dot (`error.code`); `[]` means every element of an array
// (`items[].state`; a body that is an array starts with `[]`); `{}` means every value of a map, used only where the contract's Shape says MapOf and the keys are not compared
// (`checks{}.status`). A key of RouteContract.Vocab is such a path to a string-valued position: `items[].state` lists the closed set of the `state` of every element of
// `items`. A key that is not a path, or names a position the fixture does not have, is a problem CheckFixtures reports.
//
// WHAT IT CANNOT SEE. The gate compares the responses the harness actually obtained: it cannot see response bodies for inputs the harness never sends, fields the module adds
// in states the harness never reaches, or anything the module does only in production. Coverage.Print says so, with the counts, every time.
//
// THE SHAPE SHIM. modclient.Shape has no exported accessors, and the comparison needs two facts Decode does not give back (where a MapOf is, which keys are optional); shapeview.go
// reads them with reflection and fails loudly (ErrShapeUnreadable) when modclient's internals change. An exported read-only walk on Shape would remove the shim.
package contractcheck

// Package contractcheck is the comparison behind the contract drift gate (Phase 3, B6). Every adapter lists the module responses it relies on as modclient.RouteContract
// values: the route, the HTTP status, the Shape the adapter decodes with, the closed vocabularies it maps from, and the name of a recorded response body under its
// testdata/contract directory. The adapters' unit tests decode the recorded bodies offline. The drift gate, which runs in the harness against the REAL module, makes each call
// and hands what came back to this package, which compares it with the recorded body. The package is plain Go: no network, no database, nothing here calls a module.
//
// What is in it:
//
//   - Skeleton: the structure of a body (keys, JSON types, arrays folded into one element, maps ignoring their keys where the Shape says MapOf).
//   - Compare: a live response against a fixture; the differences are StatusChanged, KeyMissing, KeyAdded, TypeChanged, VocabNew and ShapeViolation (and ContractInvalid when
//     the comparison itself cannot be made). A difference names the module, route, case and path and prints no value except a vocabulary value. A key the contract's Shape
//     declares Opt (the module omits or sets it depending on state) is never a KeyMissing or a KeyAdded: its presence on one side only is listed in Report.Optional and
//     noted ("optional per the Shape, presence not compared"), and it does not count in the distance Match ranks by; present on both sides it is compared like any key.
//     An object the Shape marks Open (modclient.Shape.Open: a bag of module-defined keys, module 1's signals) is compared the same way for the keys the Shape lists, but a key it
//     does not list that is on one side only is neither KeyMissing nor KeyAdded and adds nothing to the distance: the object's path is returned in Report.Open and noted ("open
//     object per the Shape, extra keys not compared"); the same key on both sides is compared by type.
//     A Vocab path at which the fixture and the live body carry different strings is not a difference but is returned in Report.VocabMismatch.
//   - Match and Session: which contract an observed response belongs to, and the live gate built on it. Contracts of one module, route and status are told apart by DISTANCE: the
//     differences from Compare, plus the positions that tell nothing on one side only (an empty array or a null against content), plus the vocabulary positions that hold different
//     values (a test button that answers ok, failed or unavailable differs in nothing else), each one unit; of equal distance, fewer gaps, then the first contract nothing has been
//     assigned to yet, then contract order. A contract no response was assigned to keeps a NearestMiss: how far its fixture is from the closest response of its route and status.
//   - InspectFixtures, CheckFixtures and CheckContracts: the no-rot check (every contract has a usable fixture, every fixture has a contract, nothing secret-shaped is
//     committed, and every vocabulary constrains something: its path matches a string in the contract's own fixture, and the fixture's words are in the set). CheckContracts is
//     the line an adapter's package test calls: `contractcheck.CheckContracts(t, Contracts(), fixtures, "testdata/contract")`.
//   - Normalise, Record, ParseRecorded and WriteRecorded: record mode. The harness prints each live response as a delimited, normalised block in its log; the lead feeds the
//     downloaded log to cmd/contract-fixtures, which writes the files to commit.
//   - Coverage, AssertLive and Recorder: what the harness calls, and the report of how much of the contracts it did not exercise.
//
// THE PATH GRAMMAR. A path names a position in a body: an object key is its name joined to its parent with a dot (`error.code`); `[]` means every element of an array
// (`items[].state`; a body that is an array starts with `[]`); `{}` means every value of a map, used only where the contract's Shape says MapOf and the keys are not compared
// (`checks{}.status`). THE VOCABULARIES. A key of RouteContract.Vocab is a NAME the adapter chose ("job_status") for a closed set of words; RouteContract.VocabAt says where it
// lives, as such a path to a string-valued position ("items[].state" constrains the `state` of every element of `items`), and a name with no VocabAt entry is read as a path
// itself. Every place that checks a vocabulary resolves it that way first (the live VocabNew, the distance Match ranks by, the nearest misses, the fixture checks, the fixture notes
// of Finish). A name that is no path of the body constrains nothing, silently: CheckFixtures and Finish refuse it (VocabInert) unless the fixture has no element there because an
// array, a map or a parent on the way is empty, which is a note.
//
// WHAT IT CANNOT SEE. The gate compares the responses the harness actually obtained: it cannot see response bodies for inputs the harness never sends, fields the module adds
// in states the harness never reaches, or anything the module does only in production. Coverage.Print says so, with the counts, every time.
//
// THE SHAPE WALK. The comparison needs two facts Decode does not give back (where a MapOf is, which keys are optional); it reads them through the read-only walk of
// modclient.Shape (Kind, Nullable, Fields, Elem) and fails loudly (ErrShapeUnreadable) on a kind of Shape it was not taught.
package contractcheck

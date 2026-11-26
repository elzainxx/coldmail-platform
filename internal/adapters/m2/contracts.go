package m2

import (
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// RouteCancel is the cancel route as the contracts name it: the shared body's (module 3 has the same route since Phase 3 close-out C1).
const RouteCancel = sendapi.RouteCancel

// Contracts lists every module 2 response the adapter relies on: the shared four routes, describe and cancel (all of them sendapi.Contracts with module 2's profile, which has a cancel
// route). Fixture names a file under this package's testdata/contract/ directory; the Shape of each entry is the one the adapter decodes that response with (built by the same code the
// Client is), and testdata/README.md says which bodies are recordings of the real module and which are hand-written. A contract lists a vocabulary (Vocab, with VocabAt: the path it
// constrains) only where its body holds a string for it: see sendapi.Contracts for which; cancel's error body lists the error codes at error.code, its success lists none.
func Contracts() []modclient.RouteContract { return sendapi.Contracts(Profile()) }

// PauseContracts are module 2's contracts of the Phase 4 D1 recordings (the answers of POST /v1/pause and POST /v1/resume and the 409 `job_item_cancelled` of run: the shared body's, with module 2's
// profile), DECLARED whether or not they are listed yet: Contracts lists them only once sendapi's switch (RecordedPauseFixtures) is on, and until then the harness's drift session resolves its steps
// here and compares each with a recording only when the file exists. The fixtures are under this package's testdata/contract.
func PauseContracts() []modclient.RouteContract { return sendapi.PauseContracts(Profile()) }

// PendingContracts are the contracts that are declared (PauseContracts) and not listed yet: all of them while the switch is off, none once it is on.
func PendingContracts() []modclient.RouteContract { return sendapi.PendingContracts(Profile()) }

// RecordedPauseFixtures is the value of the switch (sendapi.RecordedPauseFixtures): the recordings are installed and their contracts listed.
func RecordedPauseFixtures() bool { return sendapi.RecordedPauseFixtures() }

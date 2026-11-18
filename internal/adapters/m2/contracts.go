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

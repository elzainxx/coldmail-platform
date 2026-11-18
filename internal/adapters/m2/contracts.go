package m2

import (
	"github.com/coldmail-platform/platform/internal/adapters/sendapi"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// RouteCancel is the one route only module 2 has, as its contracts name it.
const RouteCancel = "POST /v1/cancel"

// Contracts lists every module 2 response the adapter relies on: the shared four routes and describe (sendapi.Contracts with module 2's profile) and cancel. Fixture names a file under this
// package's testdata/contract/ directory; the Shape of each entry is the one the adapter decodes that response with (the shared ones are built by the same code the Client is, cancel's is
// cancelShape), and every body is a recording of the real module (testdata/README.md). A contract lists a vocabulary (Vocab, with VocabAt: the path it constrains) only where its body
// holds a string for it: see sendapi.Contracts for which; cancel's error body lists the error codes at error.code, its success lists none.
func Contracts() []modclient.RouteContract {
	cs := sendapi.Contracts(Profile())
	codes := Profile().Vocabularies()["error_codes"]
	return append(cs,
		modclient.RouteContract{Module: modclient.M2, Route: RouteCancel, Case: "ok", Fixture: "cancel.ok.json", Status: 200, Shape: cancelShape},
		modclient.RouteContract{
			Module: modclient.M2, Route: RouteCancel, Case: "error validation_failed", Fixture: "cancel.error.validation_failed.json", Status: 400,
			Shape: sendapi.ErrorShape(), Vocab: map[string][]string{"error_codes": codes}, VocabAt: map[string]string{"error_codes": sendapi.PathErrorCode},
		},
	)
}

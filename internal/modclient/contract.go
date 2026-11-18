package modclient

// RouteContract is one module response the platform relies on: the route, the recorded body that pins its shape, and the Shape the adapter decodes it with. Every adapter lists its contracts
// once (`Contracts() []modclient.RouteContract` in its package) and decodes with exactly the Shape it lists, so the offline unit tests (which serve the recorded body), the live drift gate (which compares a real
// response with the recorded body) and the production decoder cannot disagree about what the platform expects. It is data: the comparison lives in the harness (Phase 3 B6).
type RouteContract struct {
	// Module is the module that answers.
	Module Module
	// Route is the route as the adapter names it: the method and a path template ("GET /v1/jobs/{id}"), never a real URL (a job id would be in it).
	Route string
	// Case names the recorded response when a route has several that differ in shape (for example "ok" and "empty page"); "" when there is one.
	Case string
	// Fixture is the file name of the recorded body under the adapter package's testdata/contract/ directory.
	Fixture string
	// Status is the HTTP status of the recorded response.
	Status int
	// Shape is what the adapter decodes the body with (the keys it reads and their types).
	Shape Shape
	// Vocab lists, by a name the adapter chooses, the closed set of values the adapter maps from (statuses, reasons, origins, error codes). The drift gate fails when the module's
	// advertised or observed values are not covered by it.
	Vocab map[string][]string
	// VocabAt says where each named set of Vocab lives in the response: the key is a name of Vocab, the value the JSON path of the string-valued position it constrains ("status",
	// "items.results[].state", "error.code"; the path grammar of internal/contractcheck: a.b is a key, x[] is every element of an array). A Vocab name with no VocabAt entry is
	// read as a path itself. The live drift gate checks the real response's strings at that path against the set: without a path a vocabulary constrains nothing.
	VocabAt map[string]string
}

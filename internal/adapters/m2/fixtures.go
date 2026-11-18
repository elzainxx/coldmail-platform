package m2

import (
	"embed"
	"io/fs"
)

//go:embed testdata/contract/*.json
var contractFixtures embed.FS

// ContractFixtures is the recorded body of every response in Contracts(), as files under "testdata/contract" of the returned file system. It is compiled in so that a harness binary
// that runs inside a container needs no checkout: the live drift gate (internal/contractcheck Session) compares what the real module answers with these files, and the offline unit
// tests decode them with the adapter's own code.
func ContractFixtures() fs.FS { return contractFixtures }

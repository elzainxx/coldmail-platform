// Docs/ holds documents and preserved evidence, including raw copies of module Go sources (Docs/history/audit-scratch/, kept verbatim and never edited).
// This file makes Docs/ a module of its own, with no packages, so that `go build ./...`, `go vet ./...`, `go test ./...` and `go mod tidy` at the repository root
// never compile or resolve those copies. It is the narrowest exclusion that leaves the evidence byte-for-byte untouched.
// (gofmt does not know about modules: CI runs `gofmt -l cmd internal tests`, never `gofmt -l .`.)
module example.invalid/coldmail-platform-docs

go 1.26.0

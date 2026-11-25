//go:build !faultpoints

// Package faultpoint lets the harness kill the platform process, for real, at a named point of a job's life (Phase 4 exit criterion: "a platform SIGKILL mid-job at several injected points resumes with no item lost,
// duplicated or double-charged"). In the default build, which is the production build, Hit is an empty function and the package holds no environment variable, no signal and no name: scripts/check-faultpoints.sh
// proves that every file that reads PLATFORM_FAULTPOINT carries the build tag `faultpoints`, and the harness builds its platform image with that tag (the Dockerfile's GO_TAGS argument).
package faultpoint

// Enabled is false in the default build.
const Enabled = false

// Hit marks a point where the process may be killed on purpose. It does nothing in the default build.
func Hit(name string) {}

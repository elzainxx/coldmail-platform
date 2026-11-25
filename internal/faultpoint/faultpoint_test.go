//go:build !faultpoints

package faultpoint

import "testing"

// The default build is the production build: the package is inert. (The build with the tag is proven by the harness's crash stage, which kills a real process and reads the container's exit status.)
func TestDefaultBuildIsInert(t *testing.T) {
	if Enabled {
		t.Fatal("Enabled is true in a build without the tag `faultpoints`")
	}
	t.Setenv("PLATFORM_FAULTPOINT", "confirm.before_commit") // even an armed variable does nothing here
	Hit("confirm.before_commit")                             // reaching the next line is the assertion: the process was not killed
	Hit("")
}

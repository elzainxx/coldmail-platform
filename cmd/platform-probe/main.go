// Command platform-probe is the Phase 0 end-to-end caller: the platform's role as the first caller of
// the three modules. It checks each service's health and readiness and, for the two senders, describe
// with the service token, prints one line per check and exits 0 (green), 1 (red) or 2 (configuration
// error). See Docs/probe-reference.md.
package main

import (
	"os"

	"github.com/coldmail-platform/platform/internal/probe"
)

func main() {
	os.Exit(probe.Main(os.Getenv, os.Stdout, os.Stderr))
}

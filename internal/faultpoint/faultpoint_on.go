//go:build faultpoints

package faultpoint

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// This file exists ONLY in a build made with `-tags faultpoints` (the harness's platform image: the Dockerfile's GO_TAGS argument). It reads PLATFORM_FAULTPOINT, `<name>` or `<name>:<n>`, once: the n-th time
// Hit(name) is called the process kills ITSELF with the operating system's kill (SIGKILL on Linux: no deferred function, no signal handler, no flush, the connection to the database is simply gone), which is exactly
// what a crash, an OOM kill or a pulled plug does. The harness's crash stage restarts the process without the variable and proves what survived.

// Enabled is true in a build with the tag.
const Enabled = true

var (
	once   sync.Once
	target string
	nth    int
	mu     sync.Mutex
	seen   = map[string]int{}
)

func load() {
	spec := strings.TrimSpace(os.Getenv("PLATFORM_FAULTPOINT"))
	if spec == "" {
		return
	}
	name, count, found := strings.Cut(spec, ":")
	target, nth = name, 1
	if found {
		n, err := strconv.Atoi(count)
		if err != nil || n < 1 {
			// a typo must not silently arm nothing: say so on stderr, where the harness reads the container's log
			fmt.Fprintf(os.Stderr, "faultpoint: PLATFORM_FAULTPOINT %q has no valid count (want name or name:n with n >= 1): no fault is armed\n", spec)
			target = ""
			return
		}
		nth = n
	}
	fmt.Fprintf(os.Stderr, "faultpoint: armed %q, the process will be killed on hit %d\n", target, nth)
}

// Hit marks a point where the process may be killed on purpose.
func Hit(name string) {
	once.Do(load)
	if target == "" || name != target {
		return
	}
	mu.Lock()
	seen[name]++
	n := seen[name]
	mu.Unlock()
	if n != nth {
		return
	}
	fmt.Fprintf(os.Stderr, "faultpoint: %q hit %d: killing the process\n", name, n)
	p, err := os.FindProcess(os.Getpid())
	if err == nil {
		_ = p.Kill() // SIGKILL on Unix
	}
	select {} // never continue past a fault point that was meant to kill: if Kill failed the process hangs and the harness sees a stuck container, not a green run
}

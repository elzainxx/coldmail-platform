// Command platform is the coldmail platform service. All behaviour lives in internal/app; this file only
// turns SIGINT and SIGTERM into a context cancellation and the result into an exit status.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/coldmail-platform/platform/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// After the first signal, restore the default behaviour so a second one terminates the process
	// instead of waiting for the drain.
	go func() { <-ctx.Done(); stop() }()

	err := app.Run(ctx, os.Getenv, os.Stderr)
	stop()
	if err != nil {
		// Configuration errors name variables and reasons only; every other error is already scrubbed.
		fmt.Fprintf(os.Stderr, "platform: %v\n", err)
	}
	os.Exit(app.ExitCode(err))
}

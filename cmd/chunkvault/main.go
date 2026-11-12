// Command chunkvault is a content-addressable, deduplicating file store.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := newRootCmd(os.Stdout, os.Stderr)
	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		stop()
		os.Exit(1)
	}
}

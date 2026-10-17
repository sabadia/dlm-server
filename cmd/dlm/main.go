// Command dlm is the dlm-server daemon and command-line client.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/sabadia/dlm-server/internal/cli"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, version)
	stop()
	os.Exit(code)
}

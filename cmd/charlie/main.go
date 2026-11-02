// Command charlie is the command line for the HopCharlie platform API.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/2026-Softbank-Hackathon-Charlie/cli/internal/cli"
)

// version is set at build time: go build -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	// Ctrl-C cancels the context so watches can say how to reattach.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.New(version).Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

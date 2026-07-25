// Command recall is the canonical operator surface for Recall.
//
// It is a thin transport: argument parsing, rendering, and an exit code. The
// commands live in internal/cli so they are testable without executing a
// binary, and everything they do goes through internal/app.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/marcus/recall/internal/cli"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), terminationSignals()...)
	defer stop()
	return cli.Run(ctx, cli.Env{
		Args:   os.Args[1:],
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
}

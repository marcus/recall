// Command recall is the canonical operator surface for Recall.
//
// It is a thin transport: argument parsing, rendering, and an exit code. The
// commands live in internal/cli so they are testable without executing a
// binary, and everything they do goes through internal/app.
package main

import (
	"context"
	"os"

	"github.com/marcus/recall/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), cli.Env{
		Args:   os.Args[1:],
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}))
}

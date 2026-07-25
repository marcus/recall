// Command recall is the canonical operator surface for Recall.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/marcus/recall/internal/buildinfo"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "recall:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "version") {
		_, err := fmt.Fprintf(out, "recall %s (%s)\n", buildinfo.Version, buildinfo.Commit)
		return err
	}
	return fmt.Errorf("no command given; try --version")
}

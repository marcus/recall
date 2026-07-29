// Command recall-qmd exposes a qmd collection as a Recall document source
// through the qmd command-line client.
//
// Usage:
//
//	recall-qmd          # serve the adapter protocol on stdin/stdout
//	recall-qmd -version # print the build identity and exit
//
// The command is read-only with respect to the corpus and read-only with
// respect to the index outside a refresh. Its argv allowlist admits whole
// invocation shapes rather than subcommands, so `qmd cleanup`, `qmd collection
// remove`, and `qmd init` are unreachable, and `qmd update`/`qmd embed` are
// reachable only from recall/refresh. All retrieved text is treated as untrusted
// data.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/marcus/recall/cmd/recall-qmd/qmd"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/buildinfo"
)

func main() { os.Exit(run()) }

func run() int {
	version := flag.Bool("version", false, "print the adapter identity and exit")
	flag.Parse()
	if *version {
		if _, err := fmt.Fprintf(os.Stdout, "%s %s\n", qmd.AdapterID, buildinfo.Version); err != nil {
			return 1
		}
		return 0
	}

	logger := log.New(os.Stderr, "recall-qmd: ", log.LstdFlags|log.LUTC)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := adapter.Serve(ctx, os.Stdin, os.Stdout, qmd.New(qmd.Options{})); err != nil {
		logger.Printf("serve: %v", err)
		return 1
	}
	return 0
}

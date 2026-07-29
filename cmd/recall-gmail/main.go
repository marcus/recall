// Command recall-gmail exposes Gmail as a live Recall source through the gog
// command-line client.
//
// Usage:
//
//	recall-gmail          # serve the adapter protocol on stdin/stdout
//	recall-gmail -version # print the build identity and exit
//
// The command is read-only by construction. Its Gmail client accepts only the
// search and thread-get verbs, adds gog's no-send and no-input guards, and
// treats all retrieved mail as untrusted data.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/marcus/recall/cmd/recall-gmail/gmail"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/buildinfo"
)

func main() { os.Exit(run()) }

func run() int {
	version := flag.Bool("version", false, "print the adapter identity and exit")
	flag.Parse()
	if *version {
		if _, err := fmt.Fprintf(os.Stdout, "%s %s\n", gmail.AdapterID, buildinfo.Version); err != nil {
			return 1
		}
		return 0
	}

	logger := log.New(os.Stderr, "recall-gmail: ", log.LstdFlags|log.LUTC)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := adapter.Serve(ctx, os.Stdin, os.Stdout, gmail.New(gmail.Options{})); err != nil {
		logger.Printf("serve: %v", err)
		return 1
	}
	return 0
}

// Command recall-ongoing is Recall's adapter for the ongoing project catalog:
// a live, structured source over ongoing's HTTP API, speaking
// newline-delimited JSON-RPC 2.0 on stdio.
//
// It exists so that "what is the state of X" is a query rather than a browser
// tab. ongoing already maintains the catalog — repositories, git and LOC
// metrics, td and GitHub enrichment, traffic, and transparent attention
// classifications with every reason recorded — and this adapter carries those
// reasons through into Recall unchanged.
//
// The instance is named by the source's `location`, an http or https origin.
// A non-loopback ongoing listener requires an access secret, which is read from
// ONGOING_ACCESS_SECRET in the environment and from nowhere else.
//
// Usage:
//
//	recall-ongoing          # serve the protocol on stdin/stdout
//	recall-ongoing -version # print the build identity and exit
//
// stdout carries protocol frames only. Everything this process wants to say to
// a human goes to stderr, which the core captures into diagnostics and never
// parses.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/marcus/recall/cmd/recall-ongoing/ongoing"
	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/buildinfo"
)

func main() {
	// main only decides the exit status. Everything that has to unwind — the
	// signal handler above all — lives in run, because os.Exit skips defers.
	os.Exit(run())
}

func run() int {
	version := flag.Bool("version", false, "print the adapter identity and exit")
	flag.Parse()
	if *version {
		if _, err := fmt.Fprintf(os.Stdout, "%s %s\n", ongoing.AdapterID, buildinfo.Version); err != nil {
			return 1
		}
		return 0
	}

	logger := log.New(os.Stderr, "recall-ongoing: ", log.LstdFlags|log.LUTC)

	// SIGTERM is the core's second step when a request outlives its deadline
	// and the advisory cancel went unanswered. Handling it means in-flight
	// contexts are cancelled — including the HTTP request one of them may be
	// blocked on — and the process exits before SIGKILL arrives.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := adapter.Serve(ctx, os.Stdin, os.Stdout, ongoing.New(ongoing.Options{})); err != nil {
		logger.Printf("serve: %v", err)
		return 1
	}
	return 0
}

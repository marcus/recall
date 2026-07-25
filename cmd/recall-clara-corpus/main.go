// Command recall-clara-corpus is Recall's adapter for a Clara corpus: the
// JSONL stores under a corpus data/ directory, speaking newline-delimited
// JSON-RPC 2.0 on stdio.
//
// One instance serves one store — `store = "signals"` or `store = "memory"` —
// because a signal and a memory record have different authority, different
// freshness meaning, and want different priors. Read
// cmd/recall-clara-corpus/claracorpus for what the adapter declares and why,
// and cmd/recall-clara-corpus/conformance for what the wire looks like.
//
// Usage:
//
//	recall-clara-corpus          # serve the protocol on stdin/stdout
//	recall-clara-corpus -version # print the build identity and exit
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

	"github.com/marcus/recall/cmd/recall-clara-corpus/claracorpus"
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
		if _, err := fmt.Fprintf(os.Stdout, "%s %s\n", claracorpus.AdapterID, buildinfo.Version); err != nil {
			return 1
		}
		return 0
	}

	logger := log.New(os.Stderr, "recall-clara-corpus: ", log.LstdFlags|log.LUTC)

	// SIGTERM is the core's second step when a request outlives its deadline
	// and the advisory cancel went unanswered. Handling it means in-flight
	// contexts are cancelled and the process exits before SIGKILL arrives.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := adapter.Serve(ctx, os.Stdin, os.Stdout, claracorpus.New(claracorpus.Options{})); err != nil {
		logger.Printf("serve: %v", err)
		return 1
	}
	return 0
}

// Command recall-antfly serves a local semantic document source over Recall's adapter protocol.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/marcus/recall/internal/semantic"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/buildinfo"
)

func main() {
	version := flag.Bool("version", false, "print the adapter identity and exit")
	flag.Parse()
	if *version {
		fmt.Printf("recall-antfly %s\n", buildinfo.Version)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := adapter.Serve(ctx, os.Stdin, os.Stdout, semantic.New()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

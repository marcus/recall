package td_test

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcus/recall/internal/adapters/td"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/conformance"
)

// Rerecord rewrites each case's response.jsonl from a live run:
//
//	go test ./internal/adapters/td -run TestConformance -record
//
// It is a flag rather than a separate program so the recording and the check
// are the same code path; a recorder that differed from the checker would
// record transcripts the checker rejects.
var rerecord = flag.Bool("record", false, "rewrite each case's response.jsonl from a live run")

// conformanceCases is how many transcripts the suite must hold: the eight
// required by docs/adapter-protocol.md#conformance, with the handshake's
// acceptance and rejection recorded separately. A suite that quietly lost a
// directory would otherwise still pass every case it kept.
const conformanceCases = 9

// TestConformance replays the recorded transcripts against this adapter over
// the real wire protocol.
//
// docs/adapter-protocol.md says built-in and external adapters implement one
// contract with two transports, and this is where a built-in one is held to
// that: [adapter.Serve] exposes it on a stream, and the same replay engine
// that drives an external binary drives it here. The transcripts therefore
// test the implementation the CLI uses, not a second one written to match.
//
// Each case's fixture directory is a recording of real td output, replayed
// through the adapter's own `replay` setting. A transcript that spawned td
// would be replaying a workspace that changes with every commit, and the
// recorded responses would be stale within a day.
func TestConformance(t *testing.T) {
	root := "conformance"
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read conformance directory: %v", err)
	}
	cases := 0
	for _, entry := range entries {
		if entry.IsDir() {
			cases++
		}
	}
	if cases != conformanceCases {
		t.Fatalf("expected %d conformance cases, found %d", conformanceCases, cases)
	}

	results, err := conformance.Verify(context.Background(), root, servedAdapter, conformance.Options{})
	if err != nil {
		t.Fatalf("replay suite: %v", err)
	}
	for _, res := range results {
		if *rerecord {
			path := filepath.Join(res.Dir, "response.jsonl")
			// Redacted, not verbatim: the fields this case declares volatile
			// hold a wall clock and absolute paths naming whoever recorded it,
			// and committing those puts a machine's home directory in the tree
			// under a value nothing compares.
			body := bytes.Join(append(conformance.Redact(res.Responses, res.Volatile), nil), []byte("\n"))
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			t.Logf("recorded %d responses into %s", len(res.Responses), path)
			continue
		}
		if !res.OK() {
			t.Errorf("%s", res.Report())
		}
	}
}

// servedAdapter starts one td adapter on a pipe pair, speaking the protocol an
// external adapter speaks over stdio.
func servedAdapter(ctx context.Context) (*conformance.Process, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}

	served, cancel := context.WithCancel(ctx)
	a := td.New(td.Options{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Serve returns when stdin closes or shutdown is requested. Closing
		// the write end here is what gives the harness its EOF, exactly as a
		// child process exiting would.
		_ = adapter.Serve(served, inR, outW, a)
		_ = outW.Close()
		_ = inR.Close()
	}()

	return &conformance.Process{
		Stdin:  inW,
		Stdout: outR,
		Stop: func() {
			_ = inW.Close()
			cancel()
			<-done
			_ = a.Close()
			_ = outR.Close()
		},
	}, nil
}

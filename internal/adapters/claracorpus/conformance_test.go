package claracorpus_test

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcus/recall/internal/adapters/claracorpus"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/conformance"
)

// The conformance suite drives the built-in adapter over its real wire
// transport. The compatibility command serves this same implementation.
//
// docs/adapter-protocol.md#conformance calls for recorded transcripts, and a
// transcript recorded against a second implementation would prove the fixture.
// Every case here serves the actual built-in adapter, writes request.jsonl to
// its stdin, and compares what comes back on its stdout with response.jsonl.
// Rerecord with:
//
//	go test ./internal/adapters/claracorpus -run TestConformance -record
//
// The replaying is pkg/conformance, which is the same engine behind
// `recall doctor --conformance`. That is deliberate: a suite verified by a
// second implementation would only prove the two agreed, and the transcripts
// exist to hold this adapter to what the operator's own tool will check.
//
// Every record in every fixture is invented. A conformance suite is committed
// and the owner's real memory is not — which is the same judgment that put the
// memory store's sensitivity floor at confidential.
//
// See cmd/recall-stream/conformance/FORMAT.md for the transcript format.
var rerecord = flag.Bool("record", false, "rewrite each case's response.jsonl from a live run")

// suiteRoot is the recorded suite, and requiredCases is how many directories it
// must hold. The count is asserted so a suite that quietly lost a case reports
// that rather than passing every case it kept.
const (
	suiteRoot = "conformance"

	// The eight required cases in docs/adapter-protocol.md#conformance — with
	// the handshake's two halves recorded separately — plus two this source
	// owes on its own: memory search, where the decay arithmetic and the
	// composite lineage of a generated preference live, and memory expansion,
	// where the subject history and the cross-store locator refusal live.
	requiredCases = 11
)

func TestConformance(t *testing.T) {
	suite, err := conformance.LoadSuite(suiteRoot)
	if err != nil {
		t.Fatalf("load suite: %v", err)
	}
	if len(suite) != requiredCases {
		t.Fatalf("expected %d conformance cases, found %d", requiredCases, len(suite))
	}

	for _, tr := range suite {
		t.Run(tr.Manifest.Case, func(t *testing.T) {
			res, err := conformance.Replay(t.Context(), tr, servedAdapter, conformance.Options{})
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if *rerecord {
				record(t, tr.Dir, res.Responses, res.Volatile)
				return
			}
			if !res.OK() {
				t.Error(res.Report())
			}
		})
	}
}

// TestEveryCaseIsExercisedByTheSuite guards the one thing a recorded suite
// cannot check about itself: that a case which stopped being replayed is still
// named somewhere. LoadSuite already refuses a manifest without a description,
// so this only has to hold the required set together.
func TestEveryCaseIsExercisedByTheSuite(t *testing.T) {
	required := []string{
		"handshake", "version-rejection", "search-ranked", "search-memory",
		"search-partial", "search-unavailable", "expand-details", "expand-memory",
		"expand-expired", "cancel-inflight", "shutdown",
	}
	suite, err := conformance.LoadSuite(suiteRoot)
	if err != nil {
		t.Fatalf("load suite: %v", err)
	}
	present := map[string]bool{}
	for _, tr := range suite {
		present[tr.Manifest.Case] = true
	}
	for _, name := range required {
		if !present[name] {
			t.Errorf("conformance case %q is missing from the suite", name)
		}
	}
}

// record writes the frames one replay produced.
//
// The manifest's declared response count is the contract a replay is held to,
// and pkg/conformance refuses to load a case whose recording disagrees
// with it — so a case whose shape changed cannot be re-recorded until the two
// agree again. Writing the frames here and letting the next load enforce the
// count is what keeps that check strict rather than working around it: a
// recording with the wrong number of frames fails the very next run.
func record(t *testing.T, dir string, frames [][]byte, volatile []string) {
	t.Helper()
	path := filepath.Join(dir, "response.jsonl")
	body := bytes.Join(append(conformance.Redact(frames, volatile), nil), []byte("\n"))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("recorded %d responses into %s", len(frames), path)
}

func TestRecorderRedactsMachinePathsBeforeWritingTranscripts(t *testing.T) {
	dir := t.TempDir()
	frames := [][]byte{[]byte(
		`{"jsonrpc":"2.0","id":1,"result":{"diagnostics":{"store_identity":"/Users/example/private/corpus#signals"}}}`,
	)}
	record(t, dir, frames, []string{"/result/diagnostics/store_identity"})
	body, err := os.ReadFile(filepath.Join(dir, "response.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("/Users/")) {
		t.Fatalf("recorded transcript leaked a machine path: %s", body)
	}
	if !bytes.Contains(body, []byte("volatile")) {
		t.Fatalf("recorded transcript did not use the canonical redactor: %s", body)
	}
}

// servedAdapter exposes the built-in over the same framed stream external
// adapters use. The conformance harness therefore exercises protocol serving,
// cancellation, and shutdown as well as the implementation compiled into
// Recall.
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
	a := claracorpus.New(claracorpus.Options{})
	done := make(chan struct{})
	go func() {
		defer close(done)
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

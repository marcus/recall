package main_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marcus/recall/internal/conformance"
)

// The conformance suite drives the real binary in a real process.
//
// docs/adapter-protocol.md#conformance calls for recorded transcripts, and a
// transcript recorded against an in-process fixture would prove the fixture.
// Every case here starts cmd/recall-ongoing, writes request.jsonl to its stdin,
// and compares what comes back on its stdout with response.jsonl. Rerecord
// with:
//
//	go test ./cmd/recall-ongoing -run TestConformance -record
//
// The replaying is internal/conformance, which is the same engine behind
// `recall doctor --conformance`. That is deliberate: a suite verified by a
// second implementation would only prove the two agreed, and the transcripts
// exist to hold this adapter to what the operator's own tool will check.
// Recording lives here rather than there because a transcript is evidence about
// an adapter, and the tool that writes one belongs beside the adapter that
// produced it.
//
// See cmd/recall-stream/conformance/FORMAT.md for the transcript format.
var rerecord = flag.Bool("record", false, "rewrite each case's response.jsonl from a live run")

// suiteRoot is the recorded suite, and requiredCases is how many directories it
// must hold. The count is asserted so a suite that quietly lost a case reports
// that rather than passing every case it kept.
const (
	suiteRoot = "conformance"

	// The eight required cases in docs/adapter-protocol.md#conformance — with
	// the handshake's two halves recorded separately — plus three this source
	// owes on its own: a denied instance, a stale catalog, and the honest
	// refresh of an adapter that maintains no projection.
	requiredCases = 11
)

// binPath is the adapter binary under test, built once in TestMain.
var binPath string

func TestMain(m *testing.M) {
	flag.Parse()
	dir, err := os.MkdirTemp("", "recall-ongoing-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance: temp dir:", err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "recall-ongoing")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "conformance: build:", err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestConformance(t *testing.T) {
	suite, err := conformance.LoadSuite(suiteRoot)
	if err != nil {
		t.Fatalf("load suite: %v", err)
	}
	if len(suite) != requiredCases {
		t.Fatalf("expected %d conformance cases, found %d", requiredCases, len(suite))
	}

	target := conformance.Command(binPath)
	for _, tr := range suite {
		t.Run(tr.Manifest.Case, func(t *testing.T) {
			res, err := conformance.Replay(t.Context(), tr, target, conformance.Options{})
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if *rerecord {
				// Redacted, not verbatim: a transcript must not commit the
				// recording machine's clock or paths under fields nothing
				// compares.
				record(t, tr.Dir, conformance.Redact(res.Responses, res.Volatile))
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
		"handshake", "version-rejection", "search-ranked", "search-partial",
		"search-unavailable", "search-denied", "expand-details", "expand-expired",
		"cancel-inflight", "shutdown", "health-stale",
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
// and internal/conformance refuses to load a case whose recording disagrees
// with it — so a case whose shape changed cannot be re-recorded until the two
// agree again. Writing the frames here and letting the next load enforce the
// count is what keeps that check strict rather than working around it: a
// recording with the wrong number of frames fails the very next run.
func record(t *testing.T, dir string, frames [][]byte) {
	t.Helper()
	path := filepath.Join(dir, "response.jsonl")
	if err := os.WriteFile(path, bytes.Join(append(frames, nil), []byte("\n")), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	for i, frame := range frames {
		if !json.Valid(frame) {
			t.Fatalf("frame %d is not JSON: %s", i+1, frame)
		}
	}
	t.Logf("recorded %d responses into %s", len(frames), path)
}

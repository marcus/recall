package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marcus/recall/pkg/conformance"
)

// The recorder is the replayer. It builds the real binary, drives each case
// against a real process, and either writes response.jsonl or diffs against it.
// A hand-written transcript would be a claim about this adapter; a recorded one
// is an observation of it.
//
// The qmd output the cases replay is recorded separately, by
// conformance/record-qmd-fixtures.sh. Both layers are re-recorded when qmd's
// output changes, and both diffs are read.
var record = flag.Bool("record", false, "rewrite recorded conformance responses")

// cases is asserted rather than counted from the directory so that deleting a
// case fails the test instead of silently shrinking the suite.
const cases = 16

var binPath string

func TestMain(m *testing.M) {
	flag.Parse()
	dir, err := os.MkdirTemp("", "recall-qmd-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance: temp dir:", err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "recall-qmd")
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
	if *record {
		recordSuite(t)
		return
	}
	results, err := conformance.Verify(t.Context(), "conformance",
		conformance.Command(binPath), conformance.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != cases {
		t.Fatalf("expected %d conformance cases, found %d", cases, len(results))
	}
	for _, result := range results {
		if !result.OK() {
			t.Error(result.Report())
		}
	}
}

func recordSuite(t *testing.T) {
	suite, err := conformance.LoadSuite("conformance")
	if err != nil {
		t.Fatal(err)
	}
	for _, transcript := range suite {
		transcript.Manifest.Responses = 0
		transcript.Recorded = nil
		result, err := conformance.Replay(context.Background(), transcript,
			conformance.Command(binPath), conformance.Options{})
		if err != nil {
			t.Fatalf("%s: %v", transcript.Manifest.Case, err)
		}
		if result.Stopped != "" {
			t.Fatalf("%s: %s", transcript.Manifest.Case, result.Stopped)
		}
		responses := conformance.Redact(result.Responses, transcript.Manifest.Volatile)
		body := bytes.Join(append(responses, nil), []byte("\n"))
		if err := os.WriteFile(filepath.Join(transcript.Dir, conformance.ResponseFile), body, 0o644); err != nil {
			t.Fatal(err)
		}
		transcript.Manifest.Responses = len(result.Responses)
		raw, err := json.MarshalIndent(transcript.Manifest, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, '\n')
		if err := os.WriteFile(filepath.Join(transcript.Dir, conformance.ManifestFile), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %s: %d responses", transcript.Manifest.Case, len(result.Responses))
	}
}

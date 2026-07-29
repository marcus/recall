// Package templates_test replays the shipped adapter templates against the same
// engine `recall doctor --conformance` uses.
//
// The templates are not Go, and that is the point: an external adapter is a
// process that speaks JSON-RPC on stdio, so a template written in Go against
// this module's internal packages would prove the packages rather than the
// wire, and could not be copied out of this repository at all. What still has
// to be true is that the transcripts committed beside a template describe the
// template as it stands. That is what this file checks, on every `make check`,
// so a copier inherits a suite that passes rather than one that used to.
//
// This test lives here and not inside templates/adapter-python because that
// directory is copied wholesale into someone else's tree, where an import of
// github.com/marcus/recall/pkg/conformance would not resolve and would not
// mean anything.
package templates_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/marcus/recall/pkg/conformance"
)

// pythonTemplate is the copyable template and the count of cases it must ship:
// the eight required by docs/adapter-protocol.md#conformance, with the
// handshake's version rejection recorded separately, plus the two extra
// coverage shapes — a truncated listing and a filter the adapter cannot apply —
// that the guide names and that nothing else in the tree demonstrates.
const (
	pythonTemplate = "adapter-python"
	pythonCases    = 11
)

func TestPythonTemplateConformance(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		// A skip, not a pass. The template is verifiable with nothing but a
		// Python 3 interpreter, and a machine without one has checked nothing
		// rather than confirmed anything.
		t.Skipf("python3 is not on PATH, so the Python template was not replayed: %v", err)
	}

	root, err := filepath.Abs(filepath.Join(pythonTemplate, "conformance"))
	if err != nil {
		t.Fatalf("resolve suite: %v", err)
	}
	adapter, err := filepath.Abs(filepath.Join(pythonTemplate, "recall_notes.py"))
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	if _, err := os.Stat(adapter); err != nil {
		t.Fatalf("template adapter: %v", err)
	}

	suite, err := conformance.LoadSuite(root)
	if err != nil {
		t.Fatalf("load suite: %v", err)
	}
	// A suite that quietly lost a case would still pass every case it kept, so
	// the count is asserted rather than inferred from what is on disk.
	if len(suite) != pythonCases {
		t.Fatalf("template ships %d conformance cases, want %d", len(suite), pythonCases)
	}

	results, err := conformance.Verify(context.Background(), root,
		conformance.Command(python, adapter), conformance.Options{})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	for _, res := range results {
		if !res.OK() {
			// One failing case is a failure, not an abort: someone repairing the
			// template should see every case that moved, in one pass.
			t.Errorf("%s", res.Report())
		}
	}
}

func TestPythonTemplateSelfTests(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 is not on PATH, so the Python self-tests were not run: %v", err)
	}
	cmd := exec.Command(python, "-m", "unittest", "-v", "test_recall_notes.py")
	cmd.Dir = pythonTemplate
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("template self-tests: %v\n%s", err, out)
	}
}

// The copyable README is part of the template contract. In particular, it must
// not teach implementers to return broader partial results for a filter they
// cannot evaluate when the recorded adapter correctly skips before retrieval.
func TestPythonTemplateREADMEStatesUnsupportedFilterOutcome(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(pythonTemplate, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"unsupported filter returns `skipped`",
		"`filter_unsupported` before retrieval",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("README does not state %q", want)
		}
	}
}

func TestPythonTemplateTranscriptsAreRedactedAndPortable(t *testing.T) {
	root := filepath.Join(pythonTemplate, "conformance")
	suite, err := conformance.LoadSuite(root)
	if err != nil {
		t.Fatalf("load suite: %v", err)
	}
	for _, transcript := range suite {
		t.Run(transcript.Manifest.Case, func(t *testing.T) {
			redacted := conformance.Redact(
				transcript.Recorded, transcript.Manifest.Volatile,
			)
			for i := range transcript.Recorded {
				var recordedValue, redactedValue any
				if err := json.Unmarshal(transcript.Recorded[i], &recordedValue); err != nil {
					t.Fatalf("response %d: parse recording: %v", i+1, err)
				}
				if err := json.Unmarshal(redacted[i], &redactedValue); err != nil {
					t.Fatalf("response %d: parse canonical redaction: %v", i+1, err)
				}
				if !reflect.DeepEqual(recordedValue, redactedValue) {
					t.Errorf(
						"response %d is not canonically redacted; record with the template recorder",
						i+1,
					)
				}
				if leaked := absoluteString(recordedValue); leaked != "" {
					t.Errorf("response %d leaks absolute machine path %q", i+1, leaked)
				}
			}
		})
	}
}

func absoluteString(value any) string {
	switch value := value.(type) {
	case string:
		if filepath.IsAbs(value) ||
			(len(value) >= 3 && value[1] == ':' &&
				(value[2] == '\\' || value[2] == '/')) ||
			strings.HasPrefix(value, `\\`) {
			return value
		}
	case []any:
		for _, item := range value {
			if found := absoluteString(item); found != "" {
				return found
			}
		}
	case map[string]any:
		for _, item := range value {
			if found := absoluteString(item); found != "" {
				return found
			}
		}
	}
	return ""
}

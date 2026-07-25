package conformance_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/conformance"
)

// The manifest a well-formed synthetic case carries. Tests below break one
// field at a time, because each defect it can carry is a different lie a suite
// could tell about the adapter it claims to describe.
const goodManifest = `{
  "case": "%s",
  "description": "a synthetic case",
  "flow": "lockstep",
  "placeholders": {"FIXTURE": "fixture dir", "WORKDIR": "empty dir"},
  "volatile": ["/result/checked_at"],
  "responses": 1
}`

// writeCase materializes one case directory and returns its path.
func writeCase(t *testing.T, name, manifest string, requests, responses []string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, "fixture"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(file, body string) {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
	write("manifest.json", manifest)
	write("request.jsonl", strings.Join(requests, "\n")+"\n")
	write("response.jsonl", strings.Join(responses, "\n")+"\n")
	return dir
}

var (
	oneRequest  = []string{`{"jsonrpc":"2.0","id":1,"method":"recall/health","params":{"deadline":"2099-01-01T00:00:00Z"}}`}
	oneResponse = []string{`{"jsonrpc":"2.0","id":1,"result":{"status":"healthy","checked_at":"2026-01-01T00:00:00Z"}}`}
)

func TestLoadRejectsMalformedCases(t *testing.T) {
	// Every one of these is a defect in the transcript rather than in the
	// adapter, and finding it at load time is the difference between a
	// confusing replay failure and a clear one.
	tests := []struct {
		name      string
		manifest  string
		requests  []string
		responses []string
		want      string
	}{
		{
			name:      "case name disagrees with the directory",
			manifest:  `{"case":"elsewhere","description":"d","flow":"lockstep","responses":1}`,
			requests:  oneRequest,
			responses: oneResponse,
			want:      "directory is",
		},
		{
			name:      "no description",
			manifest:  `{"case":"synthetic","description":"  ","flow":"lockstep","responses":1}`,
			requests:  oneRequest,
			responses: oneResponse,
			want:      "no description",
		},
		{
			name:      "unknown flow",
			manifest:  `{"case":"synthetic","description":"d","flow":"pipelined","responses":1}`,
			requests:  oneRequest,
			responses: oneResponse,
			want:      "the format defines only",
		},
		{
			// The manifest count is what a replay is held to, so a recording
			// that disagrees makes one of the two a lie whichever the adapter
			// matches.
			name:      "declared count disagrees with the recording",
			manifest:  `{"case":"synthetic","description":"d","flow":"lockstep","responses":2}`,
			requests:  oneRequest,
			responses: oneResponse,
			want:      "declares 2 responses",
		},
		{
			name:      "volatile is not a JSON pointer",
			manifest:  `{"case":"synthetic","description":"d","flow":"lockstep","volatile":["result/checked_at"],"responses":1}`,
			requests:  oneRequest,
			responses: oneResponse,
			want:      "must begin with",
		},
		{
			name:      "sends nothing",
			manifest:  `{"case":"synthetic","description":"d","flow":"lockstep","responses":1}`,
			requests:  []string{""},
			responses: oneResponse,
			want:      "sends nothing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeCase(t, "synthetic", tc.manifest, tc.requests, tc.responses)
			_, err := conformance.Load(dir)
			if err == nil {
				t.Fatal("expected a load failure")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadSuiteRefusesAnEmptyRoot(t *testing.T) {
	// A suite that quietly lost its cases would report a clean pass, which is
	// the one answer a conformance run must never give for having checked
	// nothing.
	if _, err := conformance.LoadSuite(t.TempDir()); err == nil {
		t.Fatal("expected an error for a root with no cases")
	}
}

func TestBindSubstitutesPlaceholders(t *testing.T) {
	requests := []string{`{"jsonrpc":"2.0","id":1,"method":"recall/initialize","params":{"workdir":"${WORKDIR}","location":"${FIXTURE}"}}`}
	dir := writeCase(t, "synthetic", strings.ReplaceAll(goodManifest, "%s", "synthetic"), requests, oneResponse)
	tr, err := conformance.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	workdir := t.TempDir()
	bound, err := tr.Bind(conformance.Bindings{Workdir: workdir})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	// Substitution is textual and happens before parsing, so the proof is that
	// the line still parses and carries the paths as values.
	var frame struct {
		Params struct {
			Workdir  string `json:"workdir"`
			Location string `json:"location"`
		} `json:"params"`
	}
	if err := json.Unmarshal(bound[0], &frame); err != nil {
		t.Fatalf("bound request is not JSON: %v", err)
	}
	if frame.Params.Workdir != workdir {
		t.Errorf("workdir = %q, want %q", frame.Params.Workdir, workdir)
	}
	if want := tr.Fixture(); frame.Params.Location != want {
		t.Errorf("location = %q, want %q", frame.Params.Location, want)
	}
}

func TestBindEscapesPathsForJSON(t *testing.T) {
	// A path containing a character JSON escapes has to be escaped on the way
	// in, or the substituted line stops parsing. This fails on a machine whose
	// temp path contains a quote or a backslash, which is a miserable thing to
	// debug from a replay failure.
	requests := []string{`{"jsonrpc":"2.0","id":1,"method":"recall/initialize","params":{"workdir":"${WORKDIR}"}}`}
	dir := writeCase(t, "synthetic", strings.ReplaceAll(goodManifest, "%s", "synthetic"), requests, oneResponse)
	tr, err := conformance.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	workdir := filepath.Join(t.TempDir(), `quote"and\backslash`)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Skipf("filesystem rejects the path under test: %v", err)
	}
	bound, err := tr.Bind(conformance.Bindings{Workdir: workdir})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	var frame struct {
		Params struct {
			Workdir string `json:"workdir"`
		} `json:"params"`
	}
	if err := json.Unmarshal(bound[0], &frame); err != nil {
		t.Fatalf("bound request is not JSON: %v", err)
	}
	if frame.Params.Workdir != workdir {
		t.Errorf("workdir = %q, want %q", frame.Params.Workdir, workdir)
	}
}

func TestBindRefusesPlaceholdersItCannotSatisfy(t *testing.T) {
	// An adapter handed a literal "${CORPUS}" would create a directory by that
	// name and the case would fail somewhere far from the cause. Whether the
	// manifest declared the token or not, the harness cannot bind it, so the
	// two spellings of the same defect are both refused here.
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{
			name:     "declared but unbindable",
			manifest: `{"case":"synthetic","description":"d","flow":"lockstep","placeholders":{"WORKDIR":"empty dir","CORPUS":"a corpus"},"responses":1}`,
			want:     "unbound placeholder ${CORPUS}",
		},
		{
			// The manifest's placeholders map is the case's claim about what it
			// needs; a claim nothing checks is decoration.
			name:     "used but undeclared",
			manifest: `{"case":"synthetic","description":"d","flow":"lockstep","placeholders":{"WORKDIR":"empty dir"},"responses":1}`,
			want:     "the manifest does not declare",
		},
	}
	requests := []string{`{"jsonrpc":"2.0","id":1,"method":"recall/initialize","params":{"workdir":"${WORKDIR}","location":"${CORPUS}"}}`}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeCase(t, "synthetic", tc.manifest, requests, oneResponse)
			tr, err := conformance.Load(dir)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			_, err = tr.Bind(conformance.Bindings{Workdir: t.TempDir()})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestBindRequiresAnAbsoluteWorkdir(t *testing.T) {
	dir := writeCase(t, "synthetic", strings.ReplaceAll(goodManifest, "%s", "synthetic"), oneRequest, oneResponse)
	tr, err := conformance.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, workdir := range []string{"", "relative/work"} {
		if _, err := tr.Bind(conformance.Bindings{Workdir: workdir}); err == nil {
			t.Errorf("workdir %q: expected an error", workdir)
		}
	}
}

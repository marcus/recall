package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The file names inside one case directory, fixed by
// docs/adapter-protocol.md#conformance.
const (
	ManifestFile = "manifest.json"
	RequestFile  = "request.jsonl"
	ResponseFile = "response.jsonl"
	FixtureDir   = "fixture"
)

// FlowLockstep is the only dispatch discipline the format defines. A manifest
// naming anything else is refused rather than replayed on a guess: a harness
// that fell back to lockstep for an unknown flow would report a pass for a case
// it never ran the way its author meant.
const FlowLockstep = "lockstep"

// Manifest is <case>/manifest.json: what the case is, and how to replay it.
//
// The protocol document fixes the file names and nothing else. Three things
// about a recorded exchange are specific to the machine that recorded it — the
// paths in its requests, the order its lines may be dispatched in, and the
// fields no adapter can reproduce — and this is where all three are settled.
type Manifest struct {
	// Case is the case directory name.
	Case string `json:"case"`
	// Description explains the behavior the transcript proves.
	Description string `json:"description"`
	// Flow is the transcript dispatch discipline.
	Flow string `json:"flow"`

	// Placeholders documents each ${NAME} token the requests carry and what a
	// harness must bind it to. It is prose for a reader; [Transcript.Bind] is
	// what enforces that every token was actually bound.
	Placeholders map[string]string `json:"placeholders"`

	// Volatile lists RFC 6901 JSON Pointers from the root of a response frame,
	// with the segment "*" matching every element of an array and every member
	// of an object. Both sides are masked before comparison.
	Volatile []string `json:"volatile"`

	// Responses is how many frames the adapter is expected to write, drain
	// included.
	Responses int `json:"responses"`
}

// Transcript is one loaded case directory.
//
// Requests and Recorded hold the raw lines, not decoded frames. Replay sends
// the recorded bytes rather than a re-encoding of them, so nothing the harness
// does to a request — key order, number formatting, string escaping — can
// change what the adapter under test actually receives.
type Transcript struct {
	// Dir is the absolute case directory.
	Dir string
	// Manifest declares how the case is replayed.
	Manifest Manifest
	// Requests are the recorded request lines.
	Requests [][]byte
	// Recorded are the expected response lines.
	Recorded [][]byte
}

// Load reads one case directory and checks it against the format.
//
// The checks here are about the transcript, not the adapter: a manifest that
// names a different case than its directory, or declares a response count its
// own recording does not have, is a defect in the suite, and finding it at load
// time is the difference between a confusing replay failure and a clear one.
func Load(dir string) (*Transcript, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("conformance: resolve %s: %w", dir, err)
	}

	raw, err := os.ReadFile(filepath.Join(abs, ManifestFile))
	if err != nil {
		return nil, fmt.Errorf("conformance: read manifest: %w", err)
	}
	var man Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return nil, fmt.Errorf("conformance: parse %s: %w", filepath.Join(abs, ManifestFile), err)
	}

	name := filepath.Base(abs)
	switch {
	case man.Case != name:
		return nil, fmt.Errorf("conformance: manifest names case %q, directory is %q", man.Case, name)
	case strings.TrimSpace(man.Description) == "":
		return nil, fmt.Errorf("conformance: case %q carries no description; a transcript nobody can read is not documentation", name)
	case man.Flow != FlowLockstep:
		return nil, fmt.Errorf("conformance: case %q declares flow %q, the format defines only %q", name, man.Flow, FlowLockstep)
	case man.Responses < 0:
		return nil, fmt.Errorf("conformance: case %q declares %d responses", name, man.Responses)
	}
	for _, p := range man.Volatile {
		if _, err := parsePointer(p); err != nil {
			return nil, fmt.Errorf("conformance: case %q: volatile %w", name, err)
		}
	}

	requests, err := readLines(filepath.Join(abs, RequestFile))
	if err != nil {
		return nil, err
	}
	recorded, err := readLines(filepath.Join(abs, ResponseFile))
	if err != nil {
		return nil, err
	}
	if len(requests) == 0 {
		return nil, fmt.Errorf("conformance: case %q sends nothing", name)
	}
	// The manifest's count is what a replay is held to, so a recording that
	// disagrees with it would make one of the two a lie no matter which the
	// adapter matched.
	if len(recorded) != man.Responses {
		return nil, fmt.Errorf("conformance: case %q declares %d responses, %s holds %d",
			name, man.Responses, ResponseFile, len(recorded))
	}

	return &Transcript{Dir: abs, Manifest: man, Requests: requests, Recorded: recorded}, nil
}

// LoadSuite reads every case directory under root, in name order.
//
// A root holding no cases is an error. A suite that quietly lost its cases
// would otherwise report a clean pass, which is the one answer a conformance
// run must never give for having checked nothing.
func LoadSuite(root string) ([]*Transcript, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("conformance: read suite: %w", err)
	}
	var out []*Transcript
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		tr, err := Load(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("conformance: %s holds no case directories", root)
	}
	return out, nil
}

// Fixture is the case's source data directory.
func (t *Transcript) Fixture() string { return filepath.Join(t.Dir, FixtureDir) }

// Bindings are the machine-specific values a transcript's placeholders stand
// for. Both are absolute paths; Workdir must be fresh, writable, and empty for
// each case and each run, because reusing one would let a second replay observe
// a warm index the recording never had.
type Bindings struct {
	// Fixture binds ${FIXTURE}.
	Fixture string
	// Workdir binds ${WORKDIR}.
	Workdir string
}

// placeholder matches a ${NAME} token. Substitution is textual and happens
// before JSON parsing, which is what lets one token stand for a whole path.
var placeholder = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)

// Bind substitutes the placeholders into every request line.
//
// A token left over after substitution is an error rather than a line sent as
// written: an adapter handed a literal "${WORKDIR}" would create a directory by
// that name and the case would fail somewhere far from the cause.
func (t *Transcript) Bind(b Bindings) ([][]byte, error) {
	if b.Fixture == "" {
		b.Fixture = t.Fixture()
	}
	if b.Workdir == "" {
		return nil, fmt.Errorf("conformance: case %q: no ${WORKDIR} binding", t.Manifest.Case)
	}
	for _, path := range []string{b.Fixture, b.Workdir} {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("conformance: case %q: binding %q is not absolute", t.Manifest.Case, path)
		}
	}

	// Paths are substituted into JSON string literals, so they are escaped as
	// JSON before they go in. A temp directory rarely needs it; a suite that
	// failed only on a machine whose paths contain a backslash or a quote would
	// be a miserable thing to debug.
	replacer := strings.NewReplacer(
		"${FIXTURE}", jsonString(b.Fixture),
		"${WORKDIR}", jsonString(b.Workdir),
	)

	usesFixture := false
	out := make([][]byte, len(t.Requests))
	for i, line := range t.Requests {
		if bytes.Contains(line, []byte("${FIXTURE}")) {
			usesFixture = true
		}
		// The manifest's placeholders map is the case's claim about what it
		// needs bound, and a claim nothing checks is decoration: a case could
		// use a token it never declared and replay anyway, leaving the next
		// harness to discover the requirement from a failure.
		for _, m := range placeholder.FindAllSubmatch(line, -1) {
			if _, declared := t.Manifest.Placeholders[string(m[1])]; !declared {
				return nil, fmt.Errorf("conformance: case %q: request %d uses ${%s}, which the manifest does not declare",
					t.Manifest.Case, i+1, m[1])
			}
		}
		bound := []byte(replacer.Replace(string(line)))
		if m := placeholder.FindSubmatch(bound); m != nil {
			return nil, fmt.Errorf("conformance: case %q: request %d carries unbound placeholder ${%s}",
				t.Manifest.Case, i+1, m[1])
		}
		out[i] = bound
	}
	if usesFixture {
		if _, err := os.Stat(b.Fixture); err != nil {
			return nil, fmt.Errorf("conformance: case %q: ${FIXTURE} binding: %w", t.Manifest.Case, err)
		}
	}
	return out, nil
}

// readLines returns the non-blank lines of a JSONL file.
func readLines(path string) ([][]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("conformance: read %s: %w", path, err)
	}
	var out [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			out = append(out, trimmed)
		}
	}
	return out, nil
}

// jsonString renders s as it would appear inside a JSON string literal, without
// the surrounding quotes.
func jsonString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string fails only on invalid UTF-8, which it
		// replaces rather than rejecting; this branch is unreachable.
		return s
	}
	return strings.Trim(string(encoded), `"`)
}

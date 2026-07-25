package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/marcus/recall/internal/cli"
)

// `recall doctor --conformance <adapter>` replays an adapter's recorded
// transcripts against its command. The adapter under test here is a two-line
// shell script rather than a real binary: what is being checked is the wiring —
// that the registration names a suite, that the suite is driven, and that a
// response which differs fails the command — and a real adapter would only make
// the test slower and the failure harder to read.

// scriptAdapter writes an executable that answers exactly one request with a
// canned frame, then exits. One request is enough and is deliberate: a script
// that answered several would depend on the shell flushing stdout between
// writes, and a conformance test that was flaky about buffering would teach
// nobody anything.
func scriptAdapter(t *testing.T, reply string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in adapter is a shell script")
	}
	path := filepath.Join(t.TempDir(), "stand-in-adapter")
	body := "#!/bin/sh\nread -r _\nprintf '%s\\n' '" + reply + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec // the test needs an executable
		t.Fatal(err)
	}
	return path
}

// suite writes one conformance case: a manifest, one request, and the response
// the adapter is expected to give.
func suite(t *testing.T, expected string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "conformance")
	dir := filepath.Join(root, "handshake")
	if err := os.MkdirAll(filepath.Join(dir, "fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"manifest.json": `{
  "case": "handshake",
  "description": "A stand-in adapter answering one request, so the replay wiring is under test rather than an adapter.",
  "flow": "lockstep",
  "placeholders": {"WORKDIR": "absolute path of a fresh, writable, empty directory"},
  "volatile": [],
  "responses": 1
}`,
		"request.jsonl":  `{"jsonrpc":"2.0","id":1,"method":"recall/initialize","params":{"protocol_version_min":1,"protocol_version_max":1,"workdir":"${WORKDIR}","source_id":"stand-in"}}`,
		"response.jsonl": expected,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const standInReply = `{"jsonrpc":"2.0","id":1,"result":{"protocol_version":1}}`

func conformanceTOML(command, root string) string {
	return `
[defaults]
profile = "work"

[adapters.stand-in]
command = "` + command + `"
freshness_modes = ["live"]
conformance = "` + root + `"

[profiles.work]
sources = []
`
}

func TestDoctorConformanceReplaysARecordedSuite(t *testing.T) {
	command := scriptAdapter(t, standInReply)
	h := newHarness(t, harnessOptions{
		userTOML: conformanceTOML(command, suite(t, standInReply)),
		adapters: fakeAdapters(map[string]*fake{}),
	})

	code, stdout, stderr := h.run("doctor", "--conformance", "stand-in")
	if code != cli.ExitOK {
		t.Fatalf("exit %d on a suite that replays as recorded\n%s\n%s", code, stdout, stderr)
	}
	contains(t, stdout, "conformance", "the check has to be named in the report")
	contains(t, stdout, "1 of 1", "the report has to say how many cases ran")

	// The source-probing checks are deliberately absent: a conformance run is a
	// statement about an adapter binary, and one that failed because a laptop
	// was asleep would be a statement about nothing.
	code, machine, _ := h.run("doctor", "--conformance", "stand-in", "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit %d from --json: %s", code, machine)
	}
	var d cli.Diagnosis
	if err := json.Unmarshal([]byte(machine), &d); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %v\n%s", err, machine)
	}
	names := map[string]string{}
	for _, c := range d.Checks {
		names[c.Name] = c.Status
	}
	if names["conformance"] != cli.CheckPass {
		t.Errorf("conformance check = %q, want pass: %s", names["conformance"], machine)
	}
	for _, unwanted := range []string{"health", "access", "freshness", "lineage"} {
		if _, present := names[unwanted]; present {
			t.Errorf("a conformance run reported the %q check, which is about this machine's sources", unwanted)
		}
	}
}

func TestDoctorConformanceFailsWhenAResponseMoved(t *testing.T) {
	// The recording says the adapter negotiates version 1; the adapter says 2.
	// A conformance run that let that pass would be worth nothing.
	command := scriptAdapter(t, `{"jsonrpc":"2.0","id":1,"result":{"protocol_version":2}}`)
	h := newHarness(t, harnessOptions{
		userTOML: conformanceTOML(command, suite(t, standInReply)),
		adapters: fakeAdapters(map[string]*fake{}),
	})

	code, stdout, _ := h.run("doctor", "--conformance", "stand-in")
	if code == cli.ExitOK {
		t.Fatalf("exit 0 for an adapter that answered differently from its recording\n%s", stdout)
	}
	contains(t, stdout, "handshake", "the failing case has to be named")
}

func TestDoctorConformanceRefusesWhatItCannotCheck(t *testing.T) {
	// Each of these is reported as a failure rather than a pass. An adapter
	// nothing verified must never come back as verified, which is the one
	// answer a conformance run may not give for having checked nothing.
	tests := map[string]struct {
		toml    string
		adapter string
		wantIn  string
	}{
		"unregistered adapter": {
			toml:    conformanceTOML("/bin/true", suite(t, standInReply)),
			adapter: "nothing-by-that-name",
			wantIn:  "no adapter by that name",
		},
		"built-in adapter": {
			toml:    conformanceTOML("/bin/true", suite(t, standInReply)),
			adapter: "fakedocs",
			wantIn:  "built in",
		},
		"adapter shipping no transcripts": {
			toml: `
[defaults]
profile = "work"

[adapters.stand-in]
command = "/bin/true"
freshness_modes = ["live"]

[profiles.work]
sources = []
`,
			adapter: "stand-in",
			wantIn:  "no conformance directory",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				userTOML: tc.toml,
				adapters: fakeAdapters(map[string]*fake{"fakedocs": {manifest: manifest()}}),
			})
			code, stdout, _ := h.run("doctor", "--conformance", tc.adapter)
			if code == cli.ExitOK {
				t.Fatalf("exit 0 having checked nothing\n%s", stdout)
			}
			contains(t, stdout, tc.wantIn, "the report has to say why nothing was checked")
		})
	}
}

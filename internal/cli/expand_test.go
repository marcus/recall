package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/cli"
	"github.com/marcus/recall/internal/recall"
)

// Expansion is stateless with respect to the query that produced the locator,
// and both surfaces report the same evidence and the same provenance.
func TestExpandHumanAndJSONCarryTheSameFacts(t *testing.T) {
	docs := &fake{
		manifest: manifest(),
		evidence: recall.ExpandResponse{
			Content:            "Cross-source fusion uses rank.\nRaw scores are never compared.",
			SourceRevision:     "rev-9",
			Provenance:         "ranking.md:1-2",
			Truncated:          true,
			TruncationBoundary: "budget",
		},
	}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": {manifest: manifest()}}),
	})

	code, human, stderr := h.run("expand", "--detail", "full", "docs:ranking.md")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	_, machine, _ := h.run("expand", "--json", "--detail", "full", "docs:ranking.md")

	var resp recall.ExpandResponse
	if err := json.Unmarshal([]byte(machine), &resp); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, machine)
	}
	for what, want := range map[string]string{
		"content":             resp.Content,
		"provenance":          resp.Provenance,
		"revision":            resp.SourceRevision,
		"truncation boundary": resp.TruncationBoundary,
	} {
		if want == "" {
			t.Fatalf("the JSON response carries no %s; the fixture no longer exercises it", what)
		}
		if !strings.Contains(human, want) {
			t.Errorf("human output is missing the %s %q\n--- human ---\n%s", what, want, human)
		}
	}
	if !strings.Contains(human, "truncated") {
		t.Error("human output does not say the evidence was truncated")
	}
}

// A locator that cannot be expanded is a source failure, never an empty
// document, and the exit code says which kind of failure it was.
func TestExpandFailures(t *testing.T) {
	tests := []struct {
		name    string
		locator string
		want    int
		message string
		why     string
	}{
		{
			name:    "unconfigured source",
			locator: "jira:PROJ-1",
			want:    cli.ExitFailed,
			message: "source_not_configured",
			why:     "a portable locator may name a source configured on another machine",
		},
		{
			name:    "above the ceiling",
			locator: "vault:secret",
			want:    cli.ExitFailed,
			message: "denied",
			why:     "a locator can be replayed after a ceiling narrowed, so permissions are enforced again here",
		},
		{
			name:    "not a locator",
			locator: "ranking.md",
			want:    cli.ExitError,
			message: "malformed locator",
			why:     "an unreadable argument is bad usage, not a statement about any source",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				userTOML: ceilingTOML,
				adapters: fakeAdapters(map[string]*fake{
					"fakedocs":  {manifest: manifest()},
					"fakevault": {manifest: manifest(), evidence: recall.ExpandResponse{Content: "the secret"}},
				}),
			})

			code, stdout, stderr := h.run("expand", tc.locator)
			if code != tc.want {
				t.Errorf("exit = %d, want %d: %s", code, tc.want, tc.why)
			}
			contains(t, stderr, tc.message, tc.why)
			if strings.Contains(stdout, "the secret") {
				t.Error("evidence was printed for a locator that could not be served")
			}
		})
	}
}

// ceilingTOML has one source above the profile's ceiling, so a request for it
// is denied rather than answered.
const ceilingTOML = `
[defaults]
profile = "work"

[[sources]]
source_uid = "01UIDDOCS"
source_id = "docs"
adapter = "fakedocs"
freshness_mode = "indexed"
sensitivity = "internal"

[[sources]]
source_uid = "01UIDVAULT"
source_id = "vault"
adapter = "fakevault"
freshness_mode = "indexed"
sensitivity = "restricted"

[profiles.work]
sources = ["docs", "vault"]
max_sensitivity = "internal"
`

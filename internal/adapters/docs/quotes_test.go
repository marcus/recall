package docs_test

import (
	"os"
	"path/filepath"
	"testing"
)

// The end-to-end shape of settings.examples_quote_queries, which is otherwise
// only asserted at the unit level: a documentation corpus that argues over a
// realistic user query stops competing, on that query, with records that ARE
// the thing — and remains answerable about itself.
func TestExamplesQuoteQueriesReachesRelevance(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "protocol.md", `# Adapter protocol

## relevance

Coverage alone cannot separate a four-term task titled "Make a dentist
appointment" from a four-hundred-term chunk that uses "no dentist appointment"
as an example. Both factors are required, because each covers a case the
other gets wrong.
`)
	write(t, root, "clinic.md", `# Dentist near the harbour

The dentist accepts the plan and is taking new patients.
`)

	for _, tc := range []struct {
		name      string
		settings  map[string]any
		quotingIs float64
	}{
		{"undeclared", map[string]any{}, 1},
		{"declared", map[string]any{"examples_quote_queries": true}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newAdapter(t, root, tc.settings)
			resp := search(t, a, "dentist")

			var quoting, own float64
			var sawQuoting, sawOwn bool
			for _, c := range resp.Candidates {
				if c.Relevance == nil {
					t.Fatalf("%s reported no relevance", c.Locator)
				}
				switch {
				case filepath.Base(c.Metadata["path"].(string)) == "protocol.md":
					quoting, sawQuoting = *c.Relevance, true
				default:
					own, sawOwn = *c.Relevance, true
				}
			}
			if !sawQuoting {
				t.Fatal("the quoting chunk was not returned at all; it must stay findable")
			}
			if !sawOwn || own == 0 {
				t.Fatal("the document that is about the query lost its relevance")
			}
			if tc.quotingIs == 0 && quoting != 0 {
				t.Errorf("a declared corpus reported relevance %v for a chunk that only quotes the query", quoting)
			}
			if tc.quotingIs != 0 && quoting == 0 {
				t.Errorf("an undeclared corpus discounted its own prose")
			}
			// Whatever the setting, the corpus stays answerable about itself:
			// the term the document asserts in its own voice is unaffected.
			if got := search(t, a, "coverage"); len(got.Candidates) == 0 {
				t.Error("a term the document asserts outside every quotation stopped matching")
			}
		})
	}
}

func write(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

package td

import (
	"strings"
	"testing"
)

// TestMultipleLabelsAreRefused. td INTERSECTS repeated --labels while the
// schema documented "any of", so two labels selected issues carrying both —
// usually none. The source then answered every query with zero candidates
// while health reported its full record_count and coverage complete: a false
// absence presented as a complete boundary, which is exactly the failure
// invariant 5 exists to prevent, arriving through configuration where nothing
// downstream can see it.
//
// Refusing at handshake is the honest option. Quietly unioning would make one
// configured filter mean something different from the same filter typed at td.
func TestMultipleLabelsAreRefused(t *testing.T) {
	_, err := parseSettings(map[string]any{
		"labels": []any{"track-adapter", "track-eval"},
	})
	if err == nil {
		t.Fatal("two labels accepted; td intersects them, so the source would answer nothing")
	}
	if !strings.Contains(err.Error(), "intersects") {
		t.Errorf("error = %q, want it to explain the intersection", err)
	}
}

// TestOneLabelIsAccepted keeps the refusal from becoming a ban on label
// scoping, which is a legitimate and useful configuration.
func TestOneLabelIsAccepted(t *testing.T) {
	set, err := parseSettings(map[string]any{"labels": []any{"v2"}})
	if err != nil {
		t.Fatalf("one label rejected: %v", err)
	}
	if len(set.Labels) != 1 || set.Labels[0] != "v2" {
		t.Errorf("labels = %v, want [v2]", set.Labels)
	}
}

// TestOverlongTermIsNotProbed. A term long enough to blow the argv limit makes
// its probe fail E2BIG, surfacing as an unavailable source and degrading an
// otherwise answerable query. Query text must not be able to move coverage.
func TestOverlongTermIsNotProbed(t *testing.T) {
	long := strings.Repeat("a", maxTermBytes+1)
	terms := queryTerms("adapter " + long + " conformance")
	for _, term := range terms {
		if len(term) > maxTermBytes {
			t.Errorf("term of %d bytes survived; it would fail exec and degrade coverage", len(term))
		}
	}
	if len(terms) != 2 {
		t.Errorf("terms = %v, want the two usable words kept", terms)
	}
}

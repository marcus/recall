package docs

import (
	"slices"
	"testing"
)

func TestAnalyzeQuery(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantRaw    []string
		wantTerms  []string
		normalized bool
	}{
		{
			name:       "natural question equals keyword form",
			query:      "What is the wifi password?",
			wantRaw:    []string{"what", "is", "the", "wifi", "password"},
			wantTerms:  []string{"wifi", "password"},
			normalized: true,
		},
		{
			name:       "exact identifier retains surrounding raw tokens",
			query:      "What is the state of td-b9d06e?",
			wantRaw:    []string{"what", "is", "the", "state", "of", "td", "b9d06e"},
			wantTerms:  []string{"state", "of", "td", "b9d06e"},
			normalized: true,
		},
		{
			name:       "double quoted phrase preserves function words",
			query:      `find "what is the state"`,
			wantRaw:    []string{"find", "what", "is", "the", "state"},
			wantTerms:  []string{"find", "what", "is", "the", "state"},
			normalized: false,
		},
		{
			name:       "all stopwords fall back for meaningful short query",
			query:      "what is",
			wantRaw:    []string{"what", "is"},
			wantTerms:  []string{"what", "is"},
			normalized: false,
		},
		{
			name:       "negation is meaningful",
			query:      "what is not deleted",
			wantRaw:    []string{"what", "is", "not", "deleted"},
			wantTerms:  []string{"not", "deleted"},
			normalized: true,
		},
		{
			name:       "non English terms pass unchanged",
			query:      "qué es la contraseña wifi",
			wantRaw:    []string{"qué", "es", "la", "contraseña", "wifi"},
			wantTerms:  []string{"qué", "es", "la", "contraseña", "wifi"},
			normalized: false,
		},
		{
			name:       "non Latin script passes unchanged",
			query:      "無線LAN パスワード",
			wantRaw:    []string{"無線lan", "パスワード"},
			wantTerms:  []string{"無線lan", "パスワード"},
			normalized: false,
		},
		{
			name:       "single function word remains searchable",
			query:      "Who",
			wantRaw:    []string{"who"},
			wantTerms:  []string{"who"},
			normalized: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := analyzeQuery(tc.query)
			if !slices.Equal(got.raw, tc.wantRaw) {
				t.Errorf("raw = %v, want %v", got.raw, tc.wantRaw)
			}
			if !slices.Equal(got.terms, tc.wantTerms) {
				t.Errorf("terms = %v, want %v", got.terms, tc.wantTerms)
			}
			if got.normalized != tc.normalized {
				t.Errorf("normalized = %v, want %v", got.normalized, tc.normalized)
			}
		})
	}
}

func TestQuestionAndKeywordFormsProduceTheSameLexicalTerms(t *testing.T) {
	question := uniqueTerms(analyzeQuery("what is the wifi password").terms)
	keywords := uniqueTerms(analyzeQuery("wifi password").terms)
	if !slices.Equal(question, keywords) {
		t.Fatalf("question terms %v != keyword terms %v", question, keywords)
	}
}

func TestContentEvidenceControlsFunctionWordRanking(t *testing.T) {
	question := analyzeQuery("what is the wifi password")

	absent := preserveRankingAfterContentMatch(&generation{postings: map[string][]posting{}}, question)
	if want := []string{"wifi", "password"}; !slices.Equal(absent.terms, want) {
		t.Fatalf("absent content terms = %v, want %v", absent.terms, want)
	}
	if !absent.normalized || absent.removed != 3 {
		t.Fatalf("absent content did not retain normalization decision: %+v", absent)
	}

	present := preserveRankingAfterContentMatch(&generation{
		postings: map[string][]posting{"wifi": {{chunk: 0, tf: 1}}},
	}, question)
	if !slices.Equal(present.terms, question.raw) {
		t.Fatalf("content match terms = %v, want full ranking query %v", present.terms, question.raw)
	}
	if present.normalized || present.removed != 0 {
		t.Fatalf("content match still reports a rewrite: %+v", present)
	}
}

// These seeded fuzz properties also run under ordinary `go test`. Query
// normalization may remove only declared unquoted English function words,
// must never invent a term, and must never erase a non-empty query entirely.
func FuzzAnalyzeQueryPreservesLexicalIntent(f *testing.F) {
	for _, seed := range []string{
		"what is the wifi password",
		`"what is" the state`,
		"to be or not to be",
		"not deleted",
		"qué es la contraseña",
		"無線LAN パスワード",
		"td-b9d06e",
		"what\x00is\x00the\x00wifi\x00password",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, query string) {
		got := analyzeQuery(query)
		if len(got.raw) > 0 && len(got.terms) == 0 {
			t.Fatalf("non-empty raw query erased: %+v", got)
		}

		remaining := append([]string(nil), got.raw...)
		for _, term := range got.terms {
			i := slices.Index(remaining, term)
			if i < 0 {
				t.Fatalf("normalizer invented %q: raw=%v terms=%v", term, got.raw, got.terms)
			}
			remaining = remaining[i+1:]
		}

		for _, token := range scanTokens(query, true) {
			if token.quoted && !slices.Contains(got.terms, token.value) {
				t.Fatalf("quoted token %q removed: raw=%v terms=%v", token.value, got.raw, got.terms)
			}
		}

		if got.normalized && got.removed <= 0 {
			t.Fatalf("normalized without a removal: %+v", got)
		}
		if !got.normalized && got.removed != 0 {
			t.Fatalf("reported removals without normalization: %+v", got)
		}
	})
}

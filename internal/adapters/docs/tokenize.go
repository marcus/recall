package docs

import (
	"strings"
	"unicode"
)

// maxTokenRunes bounds a single token. Anything longer is a base64 blob, a
// checksum, or a minified line rather than a word: it can never be typed as a
// query term, and keeping it would only grow every generation's term table.
const maxTokenRunes = 64

type scannedToken struct {
	value  string
	quoted bool
	// start is the token's rune offset in the scanned text. Excerpt selection
	// needs it to cut around a match, and deriving it from a second scan with
	// different rules is how the cut ends up in the middle of the word it was
	// supposed to show.
	start int
}

// tokenize folds text into lowercase alphanumeric tokens.
//
// There is deliberately no stemming and no synonym expansion. Both are
// source-local choices the adapter is free to make later, but both change
// ranking, and a first document adapter that cannot be reproduced term for term
// is not a baseline anything can be evaluated against. Splitting on every
// non-alphanumeric rune also gives token boundaries for free, which is what
// exact-identifier matching needs.
//
// Grammatical number is reconciled at query time instead, against the terms
// this index already holds, so a singular reaches a plural without either side
// being rewritten into a form nobody typed: see [recall.NumberVariants].
func tokenize(s string) []string {
	scanned := scanTokens(s, false)
	out := make([]string, 0, len(scanned))
	for _, token := range scanned {
		out = append(out, token.value)
	}
	return out
}

// scanTokens is the one lexical boundary implementation for indexed text,
// filters, exact identifiers, and query normalization. When quotes matter, a
// double-quoted run is marked so the query analyzer can preserve every word in
// a phrase the caller explicitly wrote.
func scanTokens(s string, quoteAware bool) []scannedToken {
	var out []scannedToken
	var b strings.Builder
	quoted := false
	start := 0

	flush := func() {
		if b.Len() == 0 {
			return
		}
		tok := b.String()
		b.Reset()
		if len([]rune(tok)) <= maxTokenRunes {
			out = append(out, scannedToken{value: tok, quoted: quoted, start: start})
		}
	}

	// Rune offsets, not the byte offsets ranging a string yields.
	at := -1
	for _, r := range s {
		at++
		if quoteAware && r == '"' {
			flush()
			quoted = !quoted
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if b.Len() == 0 {
				start = at
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return out
}

type queryAnalysis struct {
	raw                    []string
	terms                  []string
	retained               []string
	removed                int
	normalized             bool
	scoringWithScaffolding bool
}

// queryAnalyzer names what a query is turned into before it is matched. It is
// reported in every search's diagnostics and inside the generation's index
// config, so a change to matching is observable without reading this file.
//
// The number-variant suffix is deliberately part of the query analyzer and not
// of the tokenizer: nothing about the INDEX changed, and a generation built
// before this rule existed answers a variant query correctly, because the
// variant is resolved against that generation's own vocabulary at query time.
// See [recall.NumberVariants].
const queryAnalyzer = "alnum-fold+english-function-words-v2+number-variants-v1"

// analyzeQuery removes English grammatical scaffolding from lexical scoring
// while preserving the exact text semantics that need it.
//
// The list is intentionally much narrower than a general-purpose search-engine
// stopword list. It contains only articles, copular/do auxiliaries, and
// interrogatives: the grammatical shell that turns keywords into an English
// question. Pronouns, prepositions, conjunctions, modals, and intent verbs stay
// searchable because they can carry useful distinctions. In addition:
//   - quoted tokens are never removed;
//   - exact path and alias matching uses raw, unfiltered tokens;
//   - a query made entirely of function words falls back to its raw tokens;
//   - negation, demonstratives, temporal and directional words are not
//     stopwords because they can reverse or materially narrow a request;
//   - only English words are listed, so other scripts and languages pass
//     through unchanged.
//
// The search layer uses these terms to decide whether any content word matched.
// When none did, function words cannot manufacture candidates; when content did
// match, the full raw query remains the ranking input. These rules make "what
// is the wifi password" abstain with "wifi password" without admitting
// scaffolding-only chunks beside a real content hit, or making a meaningful
// short query or quoted phrase disappear.
func analyzeQuery(query string) queryAnalysis {
	scanned := scanTokens(query, true)
	analysis := queryAnalysis{
		raw:   make([]string, 0, len(scanned)),
		terms: make([]string, 0, len(scanned)),
	}
	for _, token := range scanned {
		analysis.raw = append(analysis.raw, token.value)
		if !token.quoted && isEnglishFunctionWord(token.value) {
			analysis.removed++
			continue
		}
		analysis.terms = append(analysis.terms, token.value)
	}

	// Empty lexical intent is worse than a noisy but honest short query. This
	// also preserves terse titles and names made entirely of common words.
	if len(analysis.terms) == 0 && len(analysis.raw) > 0 {
		analysis.terms = append(analysis.terms, analysis.raw...)
		analysis.removed = 0
	}
	analysis.normalized = analysis.removed > 0
	if analysis.normalized {
		analysis.retained = append([]string(nil), analysis.terms...)
	}
	return analysis
}

func isEnglishFunctionWord(token string) bool {
	switch token {
	case "a", "an", "the",
		"am", "are", "be", "been", "being", "did", "do", "does", "is", "was", "were",
		"what", "when", "where", "which", "who", "whom", "whose", "why", "how":
		return true
	default:
		return false
	}
}

// countTerms collapses a token list into term frequencies plus the total token
// count, which is the length BM25 normalizes against.
func countTerms(tokens []string) (map[string]int, int) {
	if len(tokens) == 0 {
		return nil, 0
	}
	terms := make(map[string]int, len(tokens))
	for _, t := range tokens {
		terms[t]++
	}
	return terms, len(tokens)
}

// containsSequence reports whether want appears in tokens as a contiguous run.
//
// Contiguity at token granularity is what makes an identifier match exact:
// "adr-0003" cannot match inside "adr-00031", because the two tokenize to
// different terms, and no substring test is ever performed.
func containsSequence(tokens, want []string) bool {
	if len(want) == 0 || len(want) > len(tokens) {
		return false
	}
	for i := 0; i+len(want) <= len(tokens); i++ {
		match := true
		for j, w := range want {
			if tokens[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

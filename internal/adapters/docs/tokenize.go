package docs

import (
	"strings"
	"unicode"
)

// maxTokenRunes bounds a single token. Anything longer is a base64 blob, a
// checksum, or a minified line rather than a word: it can never be typed as a
// query term, and keeping it would only grow every generation's term table.
const maxTokenRunes = 64

// tokenize folds text into lowercase alphanumeric tokens.
//
// There is deliberately no stemming and no synonym expansion. Both are
// source-local choices the adapter is free to make later, but both change
// ranking, and a first document adapter that cannot be reproduced term for term
// is not a baseline anything can be evaluated against. Splitting on every
// non-alphanumeric rune also gives token boundaries for free, which is what
// exact-identifier matching needs.
func tokenize(s string) []string {
	var out []string
	var b strings.Builder

	flush := func() {
		if b.Len() == 0 {
			return
		}
		tok := b.String()
		b.Reset()
		if len([]rune(tok)) <= maxTokenRunes {
			out = append(out, tok)
		}
	}

	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return out
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

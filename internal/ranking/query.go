package ranking

import (
	"strings"
	"unicode"
)

// Query classes are the closed vocabulary the core currently derives from a
// request. They select source intent priors and decide whether an exact match
// means the caller named a record or merely used its name in prose.
const (
	QueryClassIdentifier      = "identifier_query"
	QueryClassNaturalLanguage = "natural_language"
)

// ClassifyQuery derives the ranking intent of a request from its text.
//
// Stable identifier syntax wins over sentence shape: "What is the state of
// aaaa0001?" is still an identifier lookup. A single token is likewise the
// terse lookup shape agents and humans use for project names. Every other
// multi-token query is natural language; maintaining a verb list here would
// make "summarize braid's pipeline" rank differently from "explain braid's
// pipeline" even though both use the project name as a subject.
func ClassifyQuery(query string) string {
	terms := queryTerms(query)
	if len(terms) == 0 {
		return ""
	}
	if len(terms) == 1 || len(StableIdentifiers(query)) > 0 {
		return QueryClassIdentifier
	}
	return QueryClassNaturalLanguage
}

func queryTerms(query string) []string {
	var terms []string
	var term strings.Builder
	flush := func() {
		if term.Len() == 0 {
			return
		}
		terms = append(terms, term.String())
		term.Reset()
	}
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			term.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return terms
}

// StableIdentifiers returns the identifier-shaped tokens named by a query,
// normalized for comparison with candidate identity fields.
//
// A compact letter-number token needs enough structure to be an ID: a weak
// version word such as "v2" is ordinary prose and must not turn every other
// exact signal in the query into a partition.
func StableIdentifiers(query string) []string {
	var identifiers []string
	for _, field := range strings.Fields(query) {
		token := strings.Trim(field, `"'()[]{}<>,;!?:`)
		// A sentence-final full stop is punctuation. A full stop inside the
		// remaining token is path shape ("spec.md") and still carries identity.
		token = strings.TrimRight(token, ".")
		if token == "" {
			continue
		}
		if stableIdentifierToken(token) {
			identifiers = append(identifiers, strings.ToLower(token))
		}
	}
	return identifiers
}

func stableIdentifierToken(token string) bool {
	if strings.ContainsAny(token, `/\:@#`) || strings.Contains(token, ".") {
		return true
	}
	if underscoreIdentifier(token) {
		return true
	}

	var letters, digits, runes int
	for _, r := range token {
		runes++
		if unicode.IsLetter(r) {
			letters++
		}
		if unicode.IsDigit(r) {
			digits++
		}
	}
	// A delimiter makes the scheme visible: td-6c98c1 and REC-201 are
	// identifiers, while a prose hyphen with no digits is not.
	if strings.ContainsRune(token, '-') && letters > 0 && digits > 0 {
		return true
	}
	// Some stable IDs are compact (Tasks' aaaa0001). Require the shape to
	// be substantial enough that v2, h1, and other version words stay prose.
	return runes >= 8 && letters >= 2 && digits >= 2
}

func underscoreIdentifier(token string) bool {
	parts := strings.Split(token, "_")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' {
				return false
			}
		}
	}
	return true
}

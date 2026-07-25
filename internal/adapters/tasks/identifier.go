package tasks

import (
	"regexp"
	"strings"
)

// idPattern is the Tasks stable id: exactly eight lowercase hex digits, as
// minted by the CLI and enforced by `tasks check`.
//
// It is anchored at both ends and applied to a whole token, never searched
// inside one. An unanchored search is precisely the "unbounded substring
// match" that docs/adapter-protocol.md forbids from carrying
// `exact_identifier`.
var idPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)

// idTrimSet is the punctuation that may surround an id in prose without being
// part of it — sentence punctuation, brackets, quotes, and a leading reference
// sigil.
//
// "/", "-", and "_" are deliberately absent. They are the characters that join
// an id to something else: a URL path segment, a namespaced reference like
// "td-aaaa0002", an identifier in another scheme. Trimming them would lift an
// id out of a longer identifier, which is the false positive this whole
// function exists to prevent.
const idTrimSet = ".,;:!?()[]{}<>\"'#“”‘’«»"

// identifierTokens returns the stable ids named at token boundaries in a
// query, in order of first appearance and without duplicates.
//
// A token is a whitespace-delimited word with surrounding punctuation trimmed.
// Nothing splits inside a word, so an id embedded in a URL, in a longer word,
// or inside another identifier is never extracted, and a case variant is never
// accepted: the pattern demands lowercase because the CLI mints lowercase and
// two spellings of one id would make `exact_identifier` a fuzzy signal.
//
// A returned token is a candidate, not a fact. The caller still confirms the
// id exists and that the record the CLI resolved carries exactly that id,
// because `tasks show` also resolves title substrings and line references.
func identifierTokens(query string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, word := range strings.Fields(query) {
		token := strings.Trim(word, idTrimSet)
		if !idPattern.MatchString(token) {
			continue
		}
		if _, dup := seen[token]; dup {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

// stopwords are dropped from lexical terms. Adapters own term handling per
// docs/adapter-protocol.md — the core sends query text verbatim — so this list
// is source-local policy, kept short because a long one starts discarding real
// task vocabulary ("state", "review", "next").
var stopwords = map[string]struct{}{
	"a": {}, "about": {}, "am": {}, "an": {}, "and": {}, "any": {}, "are": {},
	"as": {}, "at": {}, "be": {}, "but": {}, "by": {}, "did": {}, "do": {},
	"does": {}, "for": {}, "from": {}, "has": {}, "have": {}, "how": {},
	"i": {}, "in": {}, "is": {}, "it": {}, "me": {}, "my": {}, "of": {},
	"on": {}, "or": {}, "our": {}, "that": {}, "the": {}, "their": {},
	"there": {}, "this": {}, "to": {}, "was": {}, "we": {}, "were": {},
	"what": {}, "when": {}, "where": {}, "which": {}, "who": {}, "why": {},
	"will": {}, "with": {}, "you": {}, "your": {},
}

// maxTerms bounds how many lexical terms one query contributes. A pasted
// paragraph must not turn into an unbounded scoring loop over the corpus.
const maxTerms = 12

// queryTerms lowercases a query and splits it into deduplicated lexical terms.
//
// Splitting is on anything that is not a letter or digit, which is coarser
// than [identifierTokens] on purpose: for matching prose, "landing-page"
// should find "landing" and "page", while for identity it must not.
func queryTerms(query string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, field := range strings.FieldsFunc(strings.ToLower(query), notAlphanumeric) {
		if len(field) < 2 {
			continue
		}
		if _, stop := stopwords[field]; stop {
			continue
		}
		if _, dup := seen[field]; dup {
			continue
		}
		seen[field] = struct{}{}
		out = append(out, field)
		if len(out) == maxTerms {
			break
		}
	}
	return out
}

func notAlphanumeric(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	default:
		return true
	}
}

// containsWord reports whether needle appears in haystack delimited by
// non-alphanumeric bytes on both sides.
//
// Both arguments are already lowercase. A multi-byte rune's bytes are all
// >= 0x80 and so count as a boundary, which is the behavior wanted: an id or a
// word abutting a non-ASCII character is still a separate token.
func containsWord(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for offset := 0; offset+len(needle) <= len(haystack); {
		found := strings.Index(haystack[offset:], needle)
		if found < 0 {
			return false
		}
		start := offset + found
		end := start + len(needle)
		if !alphanumericAt(haystack, start-1) && !alphanumericAt(haystack, end) {
			return true
		}
		offset = start + 1
	}
	return false
}

func alphanumericAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

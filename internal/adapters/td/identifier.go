package td

import (
	"regexp"
	"strings"
)

// idPattern is the td issue id: the literal prefix `td-` and exactly six
// lowercase hex digits, as minted by td's own id generator.
//
// It is anchored at both ends and applied to a whole token, never searched
// inside one. An unanchored search is precisely the "unbounded substring
// match" that docs/adapter-protocol.md forbids from carrying
// `exact_identifier`.
//
// The prefix is required even though td itself accepts a bare `369eef` and
// normalizes it. Six hex characters occur inside commit hashes, colors, and
// checksums; treating them as an issue id would make `exact_identifier` a
// guess, and this signal partitions the whole fused result set above
// everything else.
var idPattern = regexp.MustCompile(`^td-[0-9a-f]{6}$`)

// idTrimSet is the punctuation that may surround an id in prose without being
// part of it — sentence punctuation, brackets, quotes, and a leading reference
// sigil.
//
// "/", "-", and "_" are deliberately absent. They are the characters that join
// an id to something else: a URL path segment, a branch name like
// `td-369eef-adapter`, an identifier in another scheme. Trimming them would
// lift an id out of a longer identifier, which is the false positive this
// whole function exists to prevent.
const idTrimSet = ".,;:!?()[]{}<>\"'#“”‘’«»"

// identifierTokens returns the td ids named at token boundaries in a query, in
// order of first appearance and without duplicates.
//
// A token is a whitespace-delimited word with surrounding punctuation trimmed.
// Nothing splits inside a word, so an id embedded in a URL, in a branch name,
// or inside another identifier is never extracted, and a case variant is never
// accepted: td mints lowercase, and two spellings of one id would make
// `exact_identifier` a fuzzy signal.
//
// A returned token is a candidate, not a fact. The caller still confirms the
// workspace holds an issue with exactly that id.
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

// stopwords are dropped from probe terms. Adapters own term handling per
// docs/adapter-protocol.md — the core sends query text verbatim — so this list
// is source-local policy. It is kept short because a long one starts
// discarding real engineering vocabulary ("state", "review", "next"), and
// because every surviving term costs a process spawn.
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

// maxTerms bounds how many terms one query contributes before probing is
// capped. A pasted paragraph must not turn into an unbounded term list even
// before the probe cap applies.
const maxTerms = 12

// queryTerms lowercases a query and splits it into deduplicated terms.
//
// Splitting is on anything that is not a letter or digit, which is coarser
// than [identifierTokens] on purpose: for matching prose, "cross-source"
// should probe "cross" and "source", while for identity it must not.
func queryTerms(query string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, field := range strings.FieldsFunc(strings.ToLower(query), notAlphanumeric) {
		if len(field) < 3 {
			// Shorter than three characters is not a useful substring probe
			// against td: "id LIKE '%to%'" matches most of a workspace, and
			// each probe costs a process.
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

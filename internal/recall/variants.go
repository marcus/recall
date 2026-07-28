package recall

import "strings"

// Number variants are the one definition of "the same word, spelled for a
// different count", kept here beside [Relevance] and for the same reason: every
// source that matches text token by token needs it, and a rule each adapter
// spelled for itself would make "goldeneye" reach a record in one source and
// abstain in another.

// NumberVariantWeight is the share of a term's own weight that a number variant
// of it contributes to a source's LOCAL score.
//
// Half, so that a term the caller spelled the corpus's way outweighs one the
// corpus spells differently. Within one source the gate means the two spellings
// never compete — a source holding "goldeneye" never expands it, so no record
// is reached by both — and the discount is what orders a MULTI-term query:
// "goldeneye photos" against a corpus that writes "goldeneyes" and "photos"
// scores the term it has as written above the one it does not.
//
// It is deliberately not applied to [Relevance] — a plural is exactly as much
// ABOUT the query as the singular is, and relevance is the one number compared
// across sources, so discounting it there would export a ranking opinion as a
// measurement.
const NumberVariantWeight = 0.5

// minVariantRunes is the shortest token that may be derived from or derived to.
//
// Two-letter words are where regular English plural rules stop describing
// English and start manufacturing collisions: "as" would reach "ass", "us"
// would reach "use". Nothing shorter than three characters is a noun this rule
// helps with, and the derivation is only ever a lookup into a corpus's own
// vocabulary, so the cost of the bound is a missed match and the cost of
// removing it is a false one.
const minVariantRunes = 3

// NumberVariants returns the surface forms of term that differ from it only in
// grammatical number: the plural a corpus might spell it with, and the singular
// it might be a plural of.
//
// It is generation rather than stemming, and that is the whole design. A stemmer
// rewrites both sides of the comparison into a form neither the caller nor the
// corpus wrote, which is a precision cost on identifier-shaped text
// ("universal" and "universe" collapse under Porter) and an index obligation:
// every generation has to be rebuilt before a query can match. Generating
// candidate spellings and looking each one up in the vocabulary the source
// already holds has neither property. A variant that the corpus does not
// contain costs one map lookup and disappears; a variant it does contain was
// written by a person, not derived by an algorithm.
//
// Only the regular rules are here — -s, -es after a sibilant, -y/-ies. Irregular
// plurals (indices, mice, criteria) are absent because guessing them requires a
// table, and a table that is wrong in a corpus's own vocabulary is worse than a
// rule that is silent: the caller can still type the other form, which is the
// recourse a missing match leaves and a wrong match does not.
func NumberVariants(term string) []string {
	if len([]rune(term)) < minVariantRunes {
		return nil
	}
	var out []string
	add := func(v string) {
		if v == "" || v == term || len([]rune(v)) < minVariantRunes {
			return
		}
		for _, seen := range out {
			if seen == v {
				return
			}
		}
		out = append(out, v)
	}

	// term read as a singular: how a corpus would spell more than one of it.
	switch {
	case sibilantEnding(term):
		add(term + "es")
	case strings.HasSuffix(term, "y") && !vowelBeforeFinalY(term):
		add(term[:len(term)-1] + "ies")
	default:
		add(term + "s")
	}

	// term read as a plural: the singular it would have been formed from. The
	// -es case is guarded on the sibilant because the unguarded form is where
	// this rule does damage: "notes" would yield "not", which is common enough
	// in any English corpus to match everything.
	switch {
	case strings.HasSuffix(term, "ies") && len(term) >= minIESRunes:
		// Both readings, because the spelling is ambiguous and the corpus is
		// what decides: "cities" is "city" pluralized by the -y rule, while
		// "movies" and "cookies" are "movie" and "cookie" pluralized by the
		// plain -s. Offering both costs a lookup that fails.
		add(strings.TrimSuffix(term, "ies") + "y")
		add(strings.TrimSuffix(term, "s"))
	case strings.HasSuffix(term, "es") && sibilantEnding(strings.TrimSuffix(term, "es")):
		add(strings.TrimSuffix(term, "es"))
		add(strings.TrimSuffix(term, "s"))
	case strings.HasSuffix(term, "s") && !latinSingular(term) && !nonPlural[term]:
		add(strings.TrimSuffix(term, "s"))
	}
	return out
}

// minIESRunes is the shortest word the -y-to-ies reading may be tried on.
//
// Below it that reading is describing the wrong word and cannot be right:
// "ties" and "lies" are "tie" and "lie" with an -s, and the -ies reading turns
// them into "ty" and "ly". Above it both readings are possible — "cities" is
// -y, "movies" is -s — so both are offered and the corpus decides.
const minIESRunes = 6

// nonPlural names the -s words whose trailing s is not a plural marker AND
// whose stem is itself an ordinary English word, which is the pair that makes
// the derivation damaging rather than merely useless.
//
// "lens" is the case that matters: derive "len" and, in any corpus near code,
// a query for a camera lens matches every mention of a length. "news" gives
// "new" the same way. Everything else this rule can produce is a fragment —
// "analysi", "statu", "thi" — which no corpus holds, so the lookup fails and
// costs nothing.
//
// The list is deliberately two words long and is not trying to be a dictionary.
// It names the cases observed to do damage; a third one gets added when it is
// observed, not guessed at, because a long list of exceptions is a stemmer
// wearing a different hat.
var nonPlural = map[string]bool{"lens": true, "news": true}

// latinSingular reports the -s endings that are not plural markers, so nothing
// is stripped off them.
//
// "-ss" is the obvious one: "class" and "less" are not plurals and the stems
// they would yield are not words. "-is" and "-us" are the borrowed singulars —
// analysis, basis, axis, status, corpus, focus, virus — where stripping the s
// produces a fragment, and whose real plurals are formed by rules this does not
// implement anyway. The cost is one direction of one case: "menus" no longer
// reaches "menu", while "menu" still reaches "menus", so the pair stays
// connected from the singular side. That is the trade this whole rule is
// written to make — a missed match is recoverable by typing the other form, a
// false one is not.
func latinSingular(s string) bool {
	return strings.HasSuffix(s, "ss") || strings.HasSuffix(s, "is") || strings.HasSuffix(s, "us")
}

// sibilantEnding reports the endings English pluralizes with -es.
func sibilantEnding(s string) bool {
	return strings.HasSuffix(s, "s") || strings.HasSuffix(s, "x") || strings.HasSuffix(s, "z") ||
		strings.HasSuffix(s, "ch") || strings.HasSuffix(s, "sh")
}

func vowelBeforeFinalY(s string) bool {
	if len(s) < 2 {
		return false
	}
	switch s[len(s)-2] {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	default:
		return false
	}
}

// TermVariants is what one source resolved one query's terms to: the extra
// spellings it will match a term under, keyed by the term as the caller wrote
// it. A term the source holds under the caller's own spelling is absent, and a
// nil map is the ordinary case.
type TermVariants map[string][]string

// ResolveTermVariants expands a query's terms against a source's vocabulary,
// and expands ONLY the terms that vocabulary does not already hold.
//
// This gate is the whole difference between fixing a false abstention and
// widening every query. A term the corpus spells the caller's way needs no help:
// expanding it anyway is how "what is the sidecar project for" comes to match a
// project whose note says "all my projects" and nothing else — a common noun
// reaching further for no more information, which is the failure the admission
// rules in this system keep being written against. A term the corpus does not
// hold at all is the other case entirely: it contributes nothing, and on a small
// corpus with one record on the subject it is the difference between an answer
// and `outcome abstained`, which is a positive claim that nothing is known.
//
// holds is the source's own membership test over the text it searches — an
// index's posting table, the union of its records' terms. It has to be
// source-wide and not per record, or the gate reads "this record does not use
// the word" rather than "this source does not", and every record that spells it
// the other way is admitted beside the ones that spell it the caller's way.
func ResolveTermVariants(terms []string, holds func(string) bool) TermVariants {
	var out TermVariants
	// A term repeated in a query is probed once. holds() is a scan of the
	// source's whole vocabulary in every adapter that implements it, and a
	// query naming the same absent word twice would walk the corpus twice for
	// the same answer.
	probed := make(map[string]bool, len(terms))
	for _, term := range terms {
		if probed[term] {
			continue
		}
		probed[term] = true
		if holds(term) {
			continue
		}
		if v := VariantsIn(term, holds); len(v) > 0 {
			if out == nil {
				out = make(TermVariants, len(terms))
			}
			out[term] = v
		}
	}
	return out
}

// Weigh is one term's weight in a record's token-to-weight map, under the
// term's own spelling or the best resolved variant of it.
//
// Three adapters keep a record's searchable text as such a map and score a
// query by summing lookups into it. This is that lookup, written once, so
// "goldeneye" reaches "goldeneyes" identically in each of them and the discount
// is one number rather than three.
func (v TermVariants) Weigh(weights map[string]float64, term string) float64 {
	if w, ok := weights[term]; ok {
		return w
	}
	best := 0.0
	for _, variant := range v[term] {
		if w := weights[variant] * NumberVariantWeight; w > best {
			best = w
		}
	}
	return best
}

// Count is how many occurrences of term a record holds, across the spellings
// this source resolved it to. Occurrences are counted at full weight: see
// [NumberVariantWeight].
func (v TermVariants) Count(counts map[string]int, term string) int {
	n := counts[term]
	for _, variant := range v[term] {
		n += counts[variant]
	}
	return n
}

// VariantsIn resolves term against a vocabulary the source actually holds,
// returning the variant spellings present in it.
//
// present is the source's own membership test, because only the source knows
// what its vocabulary is: an index's posting table, a record's term counts, a
// weight map. It is the whole of what an adapter has to supply to gain this.
func VariantsIn(term string, present func(string) bool) []string {
	var out []string
	for _, v := range NumberVariants(term) {
		if present(v) {
			out = append(out, v)
		}
	}
	return out
}

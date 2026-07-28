// Package ranking fuses source-local candidate lists into one ordered result
// set: lineage grouping, entity clustering, corroboration, exact-match
// promotion, and diversity selection.
//
// Boundary: pure functions over [recall] types and [lineage]. No IO, no clock,
// no adapter knowledge, no storage schema, no configuration parsing. Everything
// that varies arrives in [Config] and [Request]; nothing is discovered.
//
// Two rules shape the whole package.
//
// Rank, never native score. A source's own relevance NUMBER is diagnostic only.
// Fusion reads [recall.Candidate.LocalRank], a configured prior, and
// [recall.Candidate.Relevance], so a source that emits scores in the thousands
// cannot outrank one that emits them in [0,1]. Nothing here reads LocalScore.
//
// Relevance is not an exception to that rule, it is the reason the rule needed
// a second half. Rank alone means every source's rank 1 enters the arithmetic
// identically, so a source that found nothing relevant still contributes one
// and the prior alone decides which rank 1 wins — which is a static belief
// about a source deciding a question about a record. Relevance is comparable
// where LocalScore is not because what is fixed is its DEFINITION rather than
// its scale: every source computes coverage times concentration the same way,
// against [recall.Relevance]. A source that reports none is read as 1.0.
//
// Evidence is counted once. The same record retrieved twice is one piece of
// evidence, whether it arrived twice from one source, as a declared projection
// in another source, as a second chunk of the same record, or as a content
// fingerprint match. Corroboration requires independent lineage, which
// [lineage.Independent] decides.
//
// Explanations are a by-product of scoring, not a second pass: every configured
// value that moved a result is recorded where it was applied, so an explanation
// cannot drift from the arithmetic that produced it.
package ranking

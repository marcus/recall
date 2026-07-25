// Package docs is Recall's built-in document corpus adapter: Markdown chunked
// by heading, retrieved from an adapter-owned lexical index, expanded live from
// the original file and line range.
//
// Boundary: everything here is source-local. The adapter owns chunking,
// tokenization, BM25 scoring, its index generations, and its locator format;
// it owns no identity, no prior, and no cross-source comparison. It writes only
// inside the workdir supplied at handshake.
//
// The corpus boundary is configuration rather than a rule. exclude_dirs and
// exclude_nested_repos decide which directories are walked; both are in the
// settings digest, so changing one rebuilds the index instead of answering over
// the boundary the old generation was built under; and every directory the walk
// refused is counted in health beside coverage. An exclusion nobody can see is
// a source reporting complete coverage over content inside the root the
// operator named.
//
// Retrieval is lexical only. Semantic retrieval is a later layer that has to
// earn its place in evaluation, and an embedding model in this package would
// make the first document adapter unevaluatable against the plain baseline it
// is supposed to beat. Query analysis keeps a bounded set of English
// grammatical function words from serving as the only evidence for a match.
// Once a content term matches, the full query remains the ranking input. Quoted
// terms, exact identifiers, all-function-word short queries, negation, and
// every non-English token are preserved. The analyzer version is reported in
// diagnostics and index_config so a ranking change cannot be invisible.
//
// Three rules here are invariants rather than tunable behavior:
//
//   - source_record_id is the identity of the DOCUMENT, not of the chunk. Every
//     chunk of one file carries the same value, because corroboration collapses
//     on source_uid plus source_record_id and a per-chunk value would let this
//     source corroborate itself with two halves of one paragraph.
//   - An index generation is built into a fresh directory and published by
//     renaming a pointer, only after the corpus walk completed. A build that
//     fails or is killed leaves the previous generation readable, and health
//     reports it stale. Nothing partially written is ever loadable: a
//     generation without its trailer record is rejected.
//   - exact_identifier is emitted only for a path-shaped identifier or a
//     declared alias matched at token boundaries. Prose that happens to contain
//     a file's words is a lexical match and nothing more.
package docs

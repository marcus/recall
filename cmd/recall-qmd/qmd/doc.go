// Package qmd exposes a qmd collection as a Recall document source.
//
// It is the adapter half of an integration whose other half is a third-party
// program: [qmd] indexes a Markdown tree into SQLite, offers BM25, vector, RRF
// hybrid, and cross-encoder-reranked retrieval over it, and this package shells
// out to its CLI. It runs ALONGSIDE the built-in lexical document adapter over
// the same corpus and never replaces it: the lexical baseline is what any claim
// about semantic retrieval is measured against, and it is also the path that
// still answers when qmd's 2GB of models are not installed.
//
// # What this adapter delegates, and what it recomputes
//
// Delegated to qmd: which documents match, in what order, and with which spans.
// qmd's order becomes LocalRank and its score becomes LocalScore, which is
// diagnostic and never compared across sources.
//
// Recomputed here, deliberately: Relevance. qmd's score cannot serve as it, and
// the reason is measured rather than theoretical. Under the reranked modes an
// off-corpus query returns an unrelated document at the same score a genuinely
// relevant one earns — the cross-encoder saturates — so a threshold over that
// number cannot express an admission floor and abstention built on it would be
// dishonest. Relevance is therefore measured on the one definition every source
// shares, over the text qmd returned. See [spanRelevance].
//
// # Declared limits
//
// Each of these is a place where this adapter reports less than the contract's
// ideal, stated here because a limit a caller cannot see is indistinguishable
// from a bug.
//
//   - Relevance is measured over the RETURNED SPAN, not over the whole chunk
//     qmd ranked, because the span is the only text this process has. A span is
//     denser than the section around it, so against the built-in lexical
//     adapter over one corpus this source's relevance runs slightly high on
//     long sections. Padding the length with unreturned text would break the
//     definition in the other direction, and reading every file at search time
//     would put the corpus back on the query path.
//
//   - Watermarks are counts. `collection=<name> files=<n>` for the corpus and
//     `docs=<n> vectors=<n>` for the index are derived from the source, stable
//     across a rebuild of an unchanged corpus, and identical on two machines
//     indexing one corpus — but a count does not move when one document
//     replaces another of the same size. qmd exposes no content digest for a
//     collection, and computing one here would mean walking the corpus this
//     adapter exists to delegate.
//
//   - index_generation is best-effort. qmd owns one SQLite file, publishes no
//     generation pointer, and has no generation identity to report, so the
//     value is a digest of what its index says about itself under the
//     configuration in force. It cannot distinguish two different corpora that
//     produce the same counts under the same configuration, and this adapter
//     cannot offer the atomic build-then-publish guarantee the index
//     obligations describe, because it does not perform the build.
//
//   - as_of_support is none. qmd has no time filter, and the only date
//     available for a document is a file mtime, which is a property of this
//     checkout rather than of the record. A `since` or `until` filter is
//     therefore refused as filter_unsupported rather than approximated, and
//     candidates carry no event_time.
//
//   - Layer attribution is complete for the reranker and indirect for
//     expansion. The mode setting maps to qmd's own switches — `search`,
//     `vsearch`, `query --no-rerank`, `query` — and `--no-rerank` disables the
//     reranker only: qmd applies LLM query expansion to any single-line query,
//     so hybrid and full differ by the reranker and nothing else. Expansion is
//     attributed by comparing those two against bm25 and vector, which run no
//     model over the query at all, and the expanded query strings are reported
//     per search in `expanded_queries` so a moved metric can be traced to the
//     term that moved it. qmd can bypass expansion entirely with a structured
//     query document (`lex:`/`vec:` lines); no mode here does, because a mode
//     that rewrote the caller's query into another grammar would be measuring a
//     query nobody asked.
//
//   - The workdir is unused, and the index is not inside it. The handshake
//     supplies a writable directory and the rule is that an adapter writes its
//     indexes and checkpoints only there; this adapter writes nothing at all,
//     and the store a refresh advances belongs to qmd — `~/.cache/qmd` or a
//     project-local `<corpus>/.qmd`. That is a deviation and it is stated rather
//     than hidden: deleting Recall's state directory changes nothing here, which
//     is the property the rule protects, but Recall also cannot bound where qmd
//     keeps its own index. Everything this process writes is what `qmd update`
//     and `qmd embed` write, only from a refresh, and never from a query.
//
//   - Determinism comes from replay, not from the tool. Warm repeated queries
//     against qmd are byte-identical because it caches results, but query
//     expansion is a language model and nothing about that is guaranteed.
//     Committed evaluation packs therefore configure the `replay` key and
//     replay recorded output; see [ReplayRunner].
//
// # Failure taxonomy
//
// qmd's exit status carries no outcome semantics whatsoever: it exits 0 for an
// empty result, for a collection that does not exist, and for a query it could
// not parse. Every classification is made from the output, and none of them may
// become an empty success:
//
//   - qmd missing or not executable — unreachable (spawn failure). This is what
//     an unsatisfied dependency looks like, and it degrades coverage with this
//     source named rather than quietly reducing the answer to whatever the
//     lexical adapter found.
//   - non-zero exit — unreachable, with the first scrubbed line of output.
//   - stdout that is not the promised JSON array — a broken contract. The cold
//     start is the case this exists for: qmd's first run after an install or a
//     model eviction writes a spinner and download progress to stdout, so
//     reading unparseable output as "no results" would turn a 2GB download into
//     a successful empty search. Warm-up belongs to recall/refresh.
//   - a collection indexing a different directory from the configured
//     location — unreachable, named. It is a store-identity lie: every locator
//     and every expansion would answer for a corpus this source was not
//     configured for.
//   - a request deadline or the per-invocation timeout — a timeout. A reranked
//     query that ran out of time is a degraded answer, never a lexical one.
//
// # Egress
//
// No network at query time. qmd's models run locally, and once they are cached
// nothing in this path reaches out. The one egress is the model download, which
// happens on installation or on the first recall/refresh, and which is
// documented in docs/qmd-adapter.md rather than performed silently behind a
// search.
//
// [qmd]: https://github.com/tobi/qmd
package qmd

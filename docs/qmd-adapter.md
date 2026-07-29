# QMD adapter

`recall-qmd` is Recall's optional, first-party semantic document adapter. It
speaks Recall's external adapter protocol over stdio and shells out to
[`qmd`](https://github.com/tobi/qmd), which indexes a Markdown tree into SQLite
and offers full-text, vector, hybrid, and reranked retrieval over it.

It runs **alongside** the built-in `documents` adapter over the same corpus and
never replaces it. That is the whole shape of the integration:

- The lexical adapter stays the baseline any claim about semantic retrieval is
  measured against, and stays the fallback.
- With `qmd` uninstalled or its models absent, a configured qmd source is
  reported unavailable and the query exits 3 with the source named. Document
  search keeps working.
- Both sources derive the same `source_record_id` for the same file — the
  collection-relative path — so one document reaching Recall through both
  collapses into one lineage root instead of corroborating itself.

## What it does not fix

The dentist ranking defect is a relevance-definition problem, and this adapter
does not address it. Neither does it make paraphrase results *rank*: relevance is
recomputed lexically here (see below), so a hit that shares no term with the
query measures 0 and is withheld by a non-zero `relevance_floor` unless nothing
else clears it. Making paraphrase aboutness comparable across sources is a
separate piece of work; a local cross-encoder reranker in the core is the
candidate, and this adapter exists partly to produce the data that decides
whether it is worth building.

## Install qmd and its models

`qmd` is a Node/Bun program and is installed separately:

```sh
bun install -g @tobilu/qmd     # or: npm install -g @tobilu/qmd
qmd --version
```

Index a corpus and build embeddings:

```sh
cd ~/notes
qmd collection add . --name notes --mask '**/*.md'
qmd update
qmd embed -c notes
qmd status
```

**The one egress.** `qmd embed` downloads about 2GB of GGUF model files on first
use — an embedding model, a reranker, and a query-expansion model — into
`~/.cache/qmd/models`. Nothing else in this path reaches the network: retrieval
is local inference over cached models, and the adapter itself never calls out.
Recall will not start that download behind a query; it happens when you run the
commands above, or inside an explicit `recall refresh`.

Set `QMD_CONFIG_DIR` to relocate qmd's configuration, or run `qmd init` inside a
corpus to give it a project-local index at `<corpus>/.qmd`. The adapter runs
every `qmd` invocation with the corpus as the working directory, so a
project-local index is found and Recall's own working directory cannot change
which index a source searches.

## Configure Recall

External adapter commands are trusted user configuration. Put the registration
in `$XDG_CONFIG_HOME/recall/config.toml`, normally
`~/.config/recall/config.toml`:

```toml
[adapters.qmd]
command = "recall-qmd"
freshness_modes = ["indexed"]
conformance = "/absolute/path/to/recall-qmd-conformance"

[[sources]]
# Replace this synthetic example with a unique value once, then never change it.
source_uid = "01EXAMPLEQMD0000"
source_id = "notes-semantic"
adapter = "qmd"
location = "/Users/you/notes"
location_kind = "path"
freshness_mode = "indexed"
sensitivity = "internal"
timeout_ms = 60000

[sources.settings]
collection = "notes"
mode = "hybrid"
```

`location` is the corpus directory, and it is checked against the directory the
named collection actually indexes on **every** operation. A collection
re-pointed with `qmd collection add` makes the source unavailable and says so,
rather than answering from a corpus the source was not configured for.

The conformance path is optional. When declared it must be absolute; in a source
checkout use `cmd/recall-qmd/conformance`, and release archives carry the same
checkout-relative directory.

```sh
recall doctor --conformance qmd
recall refresh --source notes-semantic
recall query "who can clean my teeth" --profile personal
```

Run the semantic source beside the lexical one so the comparison is available:

```toml
[profiles.personal]
sources = ["docs", "notes-semantic"]
max_sensitivity = "internal"
```

## Modes

`mode` selects how many of qmd's layers run. It exists so an evaluation can
attribute a gain or a regression to a layer instead of to one opaque pipeline,
and it is recorded in `index_config`, so two runs under different modes cannot be
compared by accident.

| `mode` | qmd command | Layers | Models |
|---|---|---|---|
| `bm25` | `qmd search` | SQLite FTS5 only | none |
| `vector` | `qmd vsearch` | embedding similarity | embedding |
| `hybrid` (default) | `qmd query --no-rerank` | expansion + RRF fusion | embedding, expansion |
| `full` | `qmd query` | expansion + fusion + rerank | all three |

Two things to know about the table. `bm25` needs no model at all and is the only
mode whose empty result is trustworthy: the reranked modes score an off-corpus
query's nearest document at the same value a genuinely relevant one earns, which
is why relevance is recomputed here rather than taken from qmd. And
`--no-rerank` disables the *reranker* only — qmd applies LLM query expansion to
any single-line query — so `hybrid` and `full` differ by the reranker, and the
expansion layer is attributed by comparing them against `bm25` and `vector`,
which run no model over the query.

The declared `query_modes` follow the mode, so a `bm25` instance does not
advertise `semantic` and is not eligible for a request it would answer as a
keyword search.

### Per-result attribution

In `hybrid` and `full`, every candidate carries qmd's own score trace under
`metadata.signals`: the RRF rank and score, each contributing list with its
source (`fts`/`vec`), query type, rank, weight, and backend score, and the
rerank and blended scores where the reranker ran. The expanded query strings
appear once per search in `diagnostics.expanded_queries`. That is what makes an
individual win or loss attributable per query rather than only per run.

## Relevance, coverage, and freshness

**Relevance is recomputed**, on Recall's one shared definition, over the text qmd
returned — never taken from qmd's score, which is ordinal, differs in scale per
mode, and saturates under reranking. It is never omitted: an omitted relevance
reads as 1.0, the maximum, which would let this source outrank every source that
reports honestly. It is measured over the returned span rather than the whole
chunk qmd ranked, because the span is the only text the adapter has; against the
lexical adapter over one corpus this runs slightly high on long sections.

**Coverage** is derived from `qmd status`, and the same snapshot answers both a
search and a health probe:

- Every indexed document represented by at least one vector chunk — `success`,
  coverage `complete`.
- Fewer vector chunks than documents — `partial`, health `degraded`, with the
  counts named. Candidates from a partial boundary carry no `confirmed_at`.
- No vectors at all, in a mode that needs them — `unavailable`. A search that
  can see nothing must not return an empty list.
- Counts qmd did not report — coverage `unknown`, health `degraded`.

**Watermarks are counts**: `collection=<name> files=<n>` for the corpus and
`docs=<n> vectors=<n>` for the index. They are stable across a rebuild of an
unchanged corpus and identical on two machines indexing one corpus, but a count
does not move when one document replaces another of the same size. qmd exposes no
content digest for a collection, and computing one here would mean walking the
corpus this adapter exists to delegate. `index_generation` is a digest of what
the index reports under the configuration in force, and it is best-effort for the
same reason: qmd owns one SQLite file and publishes no generation pointer.

`as_of_support` is `none`. qmd has no time filter and the only date available for
a document is a file mtime, which is a property of a checkout rather than of the
record, so a `since` or `until` filter is refused as `filter_unsupported` rather
than approximated.

## Refresh, and the cold start

`recall refresh` runs `qmd update`, then `qmd embed`, then one throwaway query
that forces the models to load. The last step is not cosmetic: qmd's first
invocation after an install or a model eviction writes a spinner and download
progress to **stdout**, ahead of the JSON `--json` promises. The adapter treats
any stdout that is not the promised JSON array as a broken contract, because
reading it as "no results" would turn a 2GB download into a successful empty
search. Paying for the warm-up in a refresh means a query never does.

Refresh is the only operation that mutates anything. The adapter's argv allowlist
admits whole invocation shapes rather than subcommands, so `qmd cleanup`, `qmd
collection remove`, `qmd init`, and `qmd mcp` are unreachable, and `qmd update`
and `qmd embed` are reachable only from a refresh — a query cannot rebuild an
index as a side effect.

## Failure taxonomy

| Condition | Outcome |
|---|---|
| `qmd` missing or not executable | `unavailable`, reason `unreachable` |
| non-zero exit | `unavailable`, with the first scrubbed line of output |
| stdout that is not a JSON array | `failed` — broken contract |
| collection indexes another directory | `unavailable`, both directories named by base name |
| collection absent from the index | `unavailable` |
| embedding mode over an index with no vectors | `unavailable` |
| request deadline or `timeout_ms` exceeded | `timeout` |
| `[]` from a complete boundary | `success` with no candidates |

qmd's exit status carries no outcome semantics: it exits 0 for an empty result,
for a collection that does not exist, and for a query it could not parse. Every
classification above is made from output, and none of them can become an empty
success.

## Settings

| Setting | Default | Meaning |
|---|---:|---|
| `collection` | — | **Required.** The qmd collection this source searches, verified against `location` on every operation |
| `binary` | `qmd` | Executable path or name |
| `mode` | `hybrid` | `bm25` \| `vector` \| `hybrid` \| `full` |
| `max_candidates` | `25` | Candidate cap before Recall fusion; a request limit below it wins |
| `timeout_ms` | `45000` | Maximum duration of one qmd invocation |
| `refresh_timeout_ms` | `900000` | Maximum duration of one refresh, including the model download |

`replay` names a directory of recorded qmd output and exists for the conformance
suite and for committed evaluation packs; it should not be set on a live source.
A replaying source verifies no store and publishes no `store_identity`.

## Determinism

Warm repeated queries against qmd are byte-identical because it caches results,
but query expansion is a language model and nothing guarantees that. Committed
evaluation packs therefore set `replay` and replay recorded output rather than
spawning qmd. The recording format and how to regenerate it are documented in
`cmd/recall-qmd/conformance/record-qmd-fixtures.sh`.

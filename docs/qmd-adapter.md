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
  collection-relative path. To be precise about what that buys: a lineage root
  is `<source_uid>:<local>`, so two sources can never share one, and the
  record-identity corroboration key is scoped by source on purpose — a record
  identifier is unique only inside the source that issued it. What the parity
  buys is the *location-scoped* rule: fusion treats two sources whose resolved
  location and `source_record_id` agree as two views of one record, so they do
  not corroborate each other for holding the same file — two indexes over one
  document are one piece of evidence, not two. Display deduplication is
  separate: the two results cluster into one when they also agree on a title,
  and both stay expandable.

## What it does not fix

Semantic retrieval fixes **vocabulary mismatch** — a question phrased in words
its answer never uses. It does not fix **relevance definition**: the problem of
saying how much a record is *about* a query once it has been found. Recall's
shared relevance measure is built from term coverage and density, and a measure
like that cannot tell a four-word task title that *is* the errand you asked
about from a four-hundred-word document that mentions the errand once — except
by accident of length. Adding a second retrieval path does not change how
candidates are measured against each other once retrieved, so if a wrong-but-
wordy document outranks a short right one today, this adapter will not reorder
them.

Paraphrase results do surface, but only in `vector` mode, and only because that
mode measures relevance on qmd's cosine similarity rather than lexically. In
`hybrid` and `full` the lexical measure stands, so a hit sharing no term with the
query measures 0 and is withheld by a non-zero `relevance_floor` unless nothing
else clears it — and worse, those two modes carry no admission floor of their own.
Making aboutness comparable across sources in general is separate work; a local
cross-encoder reranker in the core is the candidate, and this adapter exists
partly to produce the data that decides whether it is worth building.

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

The adapter manifest declares the effective `settings.binary` value as its
`qmd` executable requirement. `recall sources` shows it and `recall doctor`
preflights it; replay mode declares no qmd requirement because it spawns none.
A slash-containing relative binary such as `./bin/qmd` resolves against the
configured corpus directory, which is also qmd's process working directory;
the manifest reports that resolved absolute command.

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

## Semantic HTML through a derived mirror

HTML is opt-in for each qmd source. Configure the documents source with matching extensions and selection rules, and add these settings to its qmd companion:

```toml
[sources.settings]
collection = "notes"
mode = "vector"
derive_extensions = [".html", ".htm"]
derived_collection = "notes-html"
exclude_dirs = [".*", "node_modules"]
exclude_globs = ["**/mockups/**", "*.template.html"]
sibling_preference = [".md", ".html", ".htm"]
exclude_nested_repos = true
```

Run `recall refresh --source notes-semantic` to build the mirror. Recall creates a missing derived collection only during refresh and refuses an existing collection that points elsewhere. Every health, search, expansion, and refresh operation validates collection paths. No query or handshake creates a collection, rewrites a mirror, updates an index, or embeds files.

The mirror lives at `$XDG_STATE_HOME/recall/sources/<source_uid>/qmd-derived/<relative original path>.md`, outside the corpus (the XDG state default is `~/.local/state`). For example, `guides/storage.html` becomes `guides/storage.html.md`. The receipt `qmd-derived.json` and stable lock file `qmd-derived.json.lock` live beside the mirror in the same source directory. All profiles containing that source share these artifacts and one derived qmd collection; ordinary adapter workdirs remain profile-owned. The core passes the source directory explicitly as protocol `shared_workdir`, keyed by immutable source UID rather than display name or profile. Standalone clients that omit it retain the v0.7.0 layout: `<state>/qmd-derived/<source_uid>` for a workdir shaped as `<state>/adapters/<source_uid>`, otherwise `<workdir>/qmd-derived`, with the receipt in the workdir. An overlapping corpus/mirror path or a symlink inside the mirror is refused. These private, rebuildable artifacts do not belong in the corpus or version control.

The shared `pkg/document` extractor renders the title, heading hierarchy, lists, and table rows as Markdown. It removes scripts, styles, hidden content and other markup, and honours robots `noindex`. Any element carrying `data-noindex` excludes its entire parsed subtree from the mirror, including headings, alt text, and metadata, regardless of the attribute value. See the [HTML subtree rules](documents-adapter.md) for parsing behavior. The shared selection rules apply exclusions and ordered sibling preference before extraction; a preferred Markdown sibling prevents an HTML copy from being mirrored. Defaults match the documents adapter: excluded directories `.*` and `node_modules`, nested repositories excluded, no globs or sibling preference, 16 MiB `max_raw_bytes`, and 1 MiB `max_file_bytes` of extracted text. Only `.html` and `.htm` can currently be derived.

Derived receipts include the HTML extraction version in their configuration digest. After upgrading from an older extraction version, run `recall refresh --source <source_id>` even if the HTML bytes have not changed. Until that explicit refresh completes, queries report incomplete coverage and refuse stale derived hits. Adding or removing `data-noindex` likewise requires refresh to clean or restore mirrored text and its index.

Refresh synchronizes mirror content by hash, removes files whose originals disappeared or became excluded, and runs `qmd update -c <derived>` followed by `qmd embed -c <derived>`. The primary collection is also refreshed. Unchanged mirror bytes retain their modification times. A durable receipt is invalidated before maintenance and marked ready only after indexing and embedding finish. A failed or cancelled refresh remains incomplete after a process restart. Qmd 2.5.3 ignores the collection filter on `update` and reindexes all collections in the selected index, including their configured update hooks; `embed` is collection-scoped. Use a project-local qmd index when maintenance must be isolated from other collections.

Refresh holds an exclusive OS file lock from collection verification through receipt publication and the final health probe. Health, search, and expansion take shared locks through result validation and mapping. Competing profiles and processes using the same shared state wait within their caller deadline; cancellation while waiting leaves the receipt unchanged. Lock files are never removed or replaced. Process exit releases ownership; a running qmd invocation inherits the lock descriptor, so killing only its adapter does not admit another refresh before that invocation exits. A failed or cancelled refresh keeps the shared receipt incomplete, and a later refresh from either profile repairs it. Use a local filesystem supporting advisory file locks. These derived locks coordinate Recall operations for one source UID and shared state root. Index writes are additionally serialized by the index-wide maintenance lock described under [Refresh, and the cold start](#refresh-and-the-cold-start), taken after the derived lock, from collection creation through the last `embed`. Neither lock coordinates old binaries, manual qmd commands, or other programs writing the index. Corpus edits are outside the lock and remain subject to the before/after hash checks.

All four query modes search both collections and interleave their ranked results within the request limit. Derived hits retain the original relative `source_record_id` and configured corpus location, so Recall applies the same fusion identity rules as for Markdown. Mirror line ranges select extracted sections and are translated to original HTML source lines. Expansion re-extracts the original file, and file-opening surfaces receive its original path; neither evidence nor locators cite mirror paths. HTML heading anchors are included in candidate metadata. Qmd normalizes virtual filenames (`b.html.md` becomes `b-html.md`); the adapter maps those handles back to original names and reports collisions as extraction failures.

Health and queries check current source hashes, selection, and mirror hashes against the completed receipt. This adds a read-only corpus scan for an opted-in source. Changed, deleted, excluded, or newly `noindex` HTML is withheld from semantic results until a successful refresh; queries return partial coverage while preserving usable primary results. Expansion can read current extracted original text when embeddings are stale, but refuses files outside the current boundary. Doctor reports `derived_collection`, `derived_health`, `derived_reason` when incomplete, `mirrored_files`, `excluded_files`, `excluded_dirs`, and derived index counts. Qmd exposes index-wide vector counts, so its ordinary count-based embedding coverage limitation still applies.

With `derive_extensions` unset or empty, retrieval, refresh, and the index digest follow the existing single-collection path. With all derived settings unset, protocol transcripts, including the settings schema, remain byte-identical. Opted-in manifests additionally describe the derived settings.

## Upgrading from v0.7.0

An existing derived collection may still point at a profile-owned mirror. Recall refuses that path with both full paths and a recovery hint. A v0.7.0 receipt has corpus and settings hashes but no source ownership identity; a similar directory name is not enough proof to repoint a collection safely. Upgrade every Recall/recall-qmd process using this source before recovery, and let any old refresh finish or stop it.

The safe recovery is to choose an unused collection name in the trusted user configuration, keeping `source_uid`, `source_id`, `location`, and the primary `collection` unchanged:

```toml
[sources.settings]
collection = "notes"
mode = "vector"
derive_extensions = [".html", ".htm"]
derived_collection = "notes-html-shared"
```

Check the proposed name with `qmd collection show notes-html-shared` from the corpus directory; proceed only when it reports that exact collection missing. Then run:

```sh
recall refresh --profile work --source notes-semantic
recall query "rainwater storage" --profile work
recall query "rainwater storage" --profile docs
```

Both profiles now use the one shared mirror, receipt, and new collection. If the chosen name already indexes another path, choose another unused name; Recall will refuse it and will not repoint it. Recovery rebuilds the mirror and embeddings rather than trusting or copying an old receipt. Old collections and profile artifacts are left untouched for operator inspection. They are unused by upgraded clients; remove them only after independently verifying ownership and that no old client still needs them. Deleting only the old Recall mirror or receipt does not repair qmd's registered path.

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

### Which mode to run

**`vector` is the recommended mode beside a lexical source, and `hybrid`/`full`
are not.** Three measured reasons:

- **Only `bm25` and `vector` have an admission floor.** `vector` returns an
  empty list for a query the corpus has nothing about — a noise query does not
  clear qmd's own cosine threshold — and its cosine separates: on the corpus
  this was measured against, true paraphrases scored 0.42–0.48 while noise fell
  below the floor entirely. `hybrid` and `full` have **no
  admission floor at all**. Their scores are rank-normalized, so rank 1 is 1.0
  whatever matched: a nonsense query returns 1.0, 0.5, 0.33 down a list of
  unrelated documents. Configuring one of them beside a lexical source can flip
  an honest abstention into a confident answer, and nothing in the candidate list
  shows it.
- **Recall's own fusion is the hybrid layer.** Running the lexical adapter and a
  `vector` qmd source in one profile fuses a full-text list and an embedding list
  with priors, lineage, and corroboration the core owns and can explain. Asking
  qmd to fuse them first buys a second, opaque blend of the same two signals.
- **Latency.** Measured on a ~60-document Markdown corpus with warm model
  caches: `full` takes over 10s against Recall's default 5s query budget, so it
  times out before it answers. `vector` takes about 2.4s. Your numbers will
  vary with corpus size and hardware; the budget will not.

`hybrid` and `full` remain configurable because they are what the mode setting is
for: they are how a layer's contribution is measured. They are evaluation
instruments, not a recommended production configuration.

Two more things about the table. `bm25` needs no model at all. And `--no-rerank`
disables the *reranker* only — qmd applies LLM query expansion to any single-line
query — so `hybrid` and `full` differ by the reranker, and the expansion layer is
attributed by comparing them against `bm25` and `vector`, which run no model over
the query.

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

**Relevance has two bases, and the mode chooses.** The initialize manifest
declares which one the configured instance uses, so the resolved query plan and
evaluation report carry it without repeating it in every search diagnostic.
Relevance is never omitted under either: an omitted relevance reads as 1.0, the
maximum, which would let this source outrank every source that reports honestly.

`vector` mode uses **qmd's cosine similarity**, quantized to four fixed decimals
and clamped to [0,1] — manifest `relevance_basis: vector_similarity`. This is the one
place a native score becomes the cross-source comparable field, and it is
defensible for exactly this one: cosine similarity over a normalized embedding is
bounded, is the same quantity for every query, and separates, with a noise query
falling below qmd's own floor rather than arriving with a misleading number.
Quantization puts sub-grid differences back into the deterministic tie-break chain
instead of letting a model's eighth decimal decide an order.

Every other mode **recomputes lexically** on Recall's shared definition, over the
text qmd returned — manifest `relevance_basis: lexical_span`. It is measured over the
returned span rather than the whole chunk qmd ranked, because the span is the only
text the adapter has; against the lexical adapter over one corpus this runs
slightly high on long sections.

The split was forced by a live rollout, and the cost of not having it is worth
stating. A lexical measure cannot see paraphrase aboutness: coverage times
concentration over shared query terms is 0 when the question and the answer share
no words. Recomputing lexically in `vector` mode therefore measured 0 for every
true paraphrase, and the core's relevance floor withheld precisely the results a
semantic source exists to surface — the corpus held the answer, qmd found it, and
it was suppressed for not repeating the question's words. In `hybrid` and `full`
that measure stays, because there is no admission information in a
rank-normalized score to replace it with.

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

The index is qmd's, not Recall's. The handshake hands every adapter a writable
workdir; without derived HTML this adapter writes no projection there, and the store a refresh advances is qmd's own — `~/.cache/qmd` or
a project-local `<corpus>/.qmd`. For opted-in HTML, deleting Recall's shared state requires a refresh to rebuild the mirror and receipt before derived results can serve again. Recall cannot bound where qmd keeps its own index.

Refresh is the only operation that mutates anything. The adapter's argv allowlist
admits whole invocation shapes rather than subcommands, so `qmd cleanup`, `qmd
collection remove`, `qmd init`, and `qmd mcp` are unreachable, and `qmd update`
and `qmd embed` are reachable only from a refresh — a query cannot rebuild an
index as a side effect.

Every write to one qmd index is serialized. `qmd update` reindexes every
collection in the index it opens, not only the source's own, so two sources whose
collections share an index used to race when they refreshed together, and one
failed with an `ON CONFLICT(collection, path)` error from qmd. A refresh now
takes an exclusive OS file lock keyed on the index file `qmd status` reports,
and holds it from the first maintenance command through the last. Sources,
profiles, and adapter processes on the same index wait for each other within
their refresh deadline; a refresh whose deadline expires while waiting fails
with that deadline, like any other cancellation. A running qmd inherits the
lock, so killing only its adapter does not admit another writer early. When qmd
does not report its index path, the refresh runs no maintenance and reports the
source unavailable, because it cannot meet the lock a sibling source holds. The
lock files live in `recall/qmd-index-locks` under the user cache directory
(`~/Library/Caches` on macOS), or in `$RECALL_QMD_LOCK_DIR` when set, and are
never removed. Every process writing one index must resolve the same lock
directory: two adapters with different `$RECALL_QMD_LOCK_DIR`, `HOME`, or
`XDG_CACHE_HOME` values, or running as different users, do not block each other.
Searches and health probes take no index lock.

qmd passes the lock descriptor to anything it starts, including collection
update hooks. A hook that leaves a background process running keeps the index
locked after qmd exits, and every later refresh on that index then times out
waiting. To find the holder, run `lsof` on the files in the lock directory.

## Failure taxonomy

| Condition | Outcome |
|---|---|
| `qmd` missing or not executable | `unavailable`, reason `unreachable` |
| non-zero exit | `unavailable`, with the first scrubbed line of output |
| stdout that is not a JSON array | `failed` — broken contract |
| collection indexes another directory | `unavailable`, both full paths named |
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
| `refresh_timeout_ms` | `900000` | Fallback total refresh ceiling when a protocol client supplies no explicit refresh deadline |

### Effective refresh timeout

The source-level `[[sources]].timeout_ms` bounds the entire Recall refresh for that source: initialization, lock waiting, extraction, qmd maintenance, and the final probe. Recall sends that deadline to the adapter. An explicit protocol refresh deadline takes precedence over `settings.refresh_timeout_ms`; raising only that setting does not extend a CLI refresh. Each qmd subprocess is also bounded by `settings.timeout_ms` (45 seconds by default), or the remaining caller deadline, whichever is sooner. When a standalone protocol client omits a refresh deadline, `settings.refresh_timeout_ms` supplies the total ceiling, still bounded by the parent context and per-invocation timeout.

For a cold refresh that takes about 71 seconds, allow sufficient time at both levels. For example, in a complete source entry:

```toml
[[sources]]
source_uid = "01EXAMPLEQMD0000"
source_id = "notes-semantic"
adapter = "qmd"
location = "/Users/you/notes"
location_kind = "path"
freshness_mode = "indexed"
sensitivity = "internal"
timeout_ms = 180000 # total source refresh, including waiting and startup

[sources.settings]
collection = "notes"
mode = "vector"
derive_extensions = [".html"]
derived_collection = "notes-html-shared"
timeout_ms = 120000 # each qmd invocation, inside the 180-second total
refresh_timeout_ms = 900000 # only the no-deadline protocol fallback
```

Run `recall refresh --profile work --source notes-semantic`. This allows at most 180 seconds overall, with each invocation capped at 120 seconds or the remaining total. A 60-second source timeout still stops a 71-second refresh even with a 15-minute `refresh_timeout_ms`. Query budgets remain separate; increasing the source ceiling does not extend a query's end-to-end latency budget.

A large source `timeout_ms` also does not lengthen the query-time health probe beyond 10 seconds, because every source's search waits for the slowest probe. If `qmd status` and `collection show` take longer than that on a busy machine, typically with a derived mirror, set `probe_timeout_ms` on the source and a `--budget-ms` or `defaults.budget_ms` that leaves room for it:

```toml
probe_timeout_ms = 20000 # query-time health probe only
```

`replay` names a directory of recorded qmd output and exists for the conformance
suite and for committed evaluation packs; it should not be set on a live source.
A replaying source verifies no store and publishes no `store_identity`.

## Determinism

Warm repeated queries against qmd are byte-identical because it caches results,
but query expansion is a language model and nothing guarantees that. Committed
evaluation packs therefore set `replay` and replay recorded output rather than
spawning qmd. The recording format and how to regenerate it are documented in
`cmd/recall-qmd/conformance/record-qmd-fixtures.sh`.

## Optional HTTP inference (including Rook)

Recall works without Rook. Leave every `inference_*` setting unset to keep the
original qmd executable, models, index discovery, and search behavior. Recall
never discovers or probes Rook on that path. The HTTP path is an explicit source
setting and does not introduce a dependency in Recall's core, CLI, or MCP.

The first HTTP path supports **vector search without query expansion**. This is
an intentional difference from the installed qmd CLI: qmd 2.8.3 `vsearch` also
runs its expansion model, despite the embeddings-only shorthand above. The HTTP
path calls qmd's cosine vector search directly and keeps its 0.3 admission floor.
It never loads a local model. `hybrid`, `full`, `bm25`, and `replay` with HTTP
inference are refused at configuration time. Expansion and reranking through a
remote provider require a separate completed contract before those modes can be
enabled. Existing local qmd modes retain their current semantics.

The bridge calls the installed qmd modules for indexing, token chunking, vector
storage/search and snippets. There is no fork or second search/index engine.
Currently supported: qmd >=2.8 and <3, tested with 2.8.3. Before opening an index,
the bridge also checks required exports, exact embedding prefixes, chunking
constants, and the snippet formatter. An incompatible package makes the source
unavailable. After updating qmd, run this cheap check, which needs no inference,
network access, model files, corpus or index:

```sh
recall-qmd --check-inference-package /absolute/path/to/node_modules/@tobilu/qmd
```

The result is JSON, including `compatible`, `qmd_version`, `tested_version`, and
`format`. `--node /absolute/path/to/node` selects a different Node executable.
Use this smoke after weekly qmd updates before treating the provider as ready.
Standalone qmd configurations do not need this bridge or Node preflight.

Example settings for a source whose existing qmd collection is named `notes`:

```toml
[sources.settings]
collection = "notes"
mode = "vector"
inference_provider = "openai"
inference_url = "https://inference.example"
inference_tokenizer_url = "https://tokenizer.example" # defaults to inference_url
inference_model = "embeddinggemma-300m-q8-0-CONTENT_DIGEST"
inference_dimensions = 768
inference_qmd_package = "/absolute/path/to/node_modules/@tobilu/qmd"
inference_index = "/absolute/path/to/a-new-provider-index.sqlite"
inference_api_key_env = "INFERENCE_API_KEY" # optional; variable name, never a key
# inference_node = "node"
# inference_rook = true
```

`inference_url` is the service root. The bridge posts standard OpenAI requests
to `/v1/embeddings`: `model`, an array of already formatted `input` strings, and
`encoding_format: "float"`. The response must echo the exact immutable model
identity and contain one indexed vector per input, with finite numbers, the
configured dimensions, and L2 normalization. The service must not add prefixes.
The current formatting contract is `qmd-gemma-v1`:

- Query: `task: search result | query: QUERY`
- Document: `title: TITLE | text: CHUNK`, using `none` when no title exists.

Token chunking uses the same model remotely. `/v1/tokenize` accepts
`{model, content}` and returns `{model, tokens}`; `/v1/detokenize` accepts
`{model, tokens}` and returns `{model, content}`. No special tokens are added by
the service. The embedding model and tokenizer must share the immutable model
identity. This first version uses Gemma formatting for any explicitly configured
model; other model prompt formats need a new named formatting contract.

For Rook, set `inference_rook = true`. The adapter adds
`metadata.rook = {localOnly: true, priority: "background", maxWaitSeconds: 0}` to
embedding and tokenizer requests. This keeps requests local and prevents an
implicit paid fallback. Generic HTTP providers receive no Rook metadata. The
optional API key is read only from the named environment variable and sent as a
Bearer header to the configured services. Inject it into Recall's process with
your local secret manager; never store it in the TOML file. Redirects are refused
so credentials and corpus text cannot follow a service to another address.
Provider errors report safe status/contract messages, never response bodies.

### Index transition

Choose an unused, explicit `inference_index`. The bridge reads the collection
from qmd's project-local configuration, or its ordinary global configuration;
it initializes only the selected primary collection and Recall's opted-in
derived collection. Explicit refresh uses qmd's indexing routines for those
collections and honours their ignore patterns. This bridge does not run qmd
collection update shell hooks. Existing non-provider qmd refresh behavior stays
unchanged.

Run `recall refresh --source notes-semantic` to build the new index. Health and
query never create a missing provider index. Refresh pins the exact immutable
model, dimensions, formatting, tokenizer version and qmd embedding fingerprint
in the index's `store_config`. A mismatched pin, unpinned existing vectors,
foreign embedding identities or a wrong vector-table dimension are refused
before qmd can migrate, repair or replace that index. Refresh never clears or
adopts incompatible vectors. Choose another unused index and explicitly refresh
when changing embedding spaces. Endpoint and credential changes alone do not
change the embedding space.

Keep the original index and model files until the new provider path has passed
a dedicated small-corpus proof and the intended corpus has been explicitly
reindexed. Query and expand a known locator before switching profiles. Failed
provider requests remain coverage failures; they cannot become an empty search
success. The same source adapter serves CLI, HTTP and MCP through Recall's
existing shared application layer. Ranking, fusion, evidence expansion and
locator contracts are unchanged.

The default test suite mocks HTTP and does not load local models. To run the
upstream qmd SDK journey with mocked inference over a temporary corpus/index:

```sh
RECALL_QMD_HTTP_PACKAGE=/absolute/path/to/node_modules/@tobilu/qmd \
  go test ./cmd/recall-qmd/qmd -run TestHTTPInferenceQMDJourney -count=1
```

External qmd CLI tests require `RECALL_QMD_LIVE=1`; model-backed cases additionally
require cached models or `RECALL_QMD_LIVE_MODELS=1`. They never use a home index.

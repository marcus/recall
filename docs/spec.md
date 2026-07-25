# Recall Specification

**Version:** 0.1 — normative

Recall is a portable retrieval layer for personal AI agents. It searches
heterogeneous, user-controlled sources and returns compact, source-grounded
evidence with stable provenance.

Recall is a standalone product. Clara, Tasks, and td are consumers and source
examples. None of their schemas, directory layouts, or ranking assumptions
belong in the core.

Companion documents: [adapter protocol](adapter-protocol.md),
[evaluation](evaluation.md), [profile example](profile-example.md).

## Scope

v1 delivers:

- One query interface over multiple logical sources.
- Declarative configuration usable from unrelated projects.
- Source-local retrieval preserved; no forced common index.
- Rank-based cross-source fusion over declared evidence lineage.
- Compact results with stable locators and progressive expansion.
- Honest reporting of missing, stale, denied, and unhealthy sources.
- Built-in and out-of-process adapters.
- A CLI, with local HTTP and MCP as thin transports over one application core.
- A deterministic retrieval benchmark.

v1 excludes:

- Writing, editing, or synchronizing source data. Recall is read-only.
- Deciding what becomes durable memory.
- A universal knowledge graph or single canonical schema.
- A learned ranking model or learned source router.
- General data synchronization or ETL.
- Per-record access control lists.

## Invariants

These hold everywhere. A violation is a defect, not a tuning choice.

1. Raw relevance scores from different sources are never compared or normalized
   onto a shared scale. Cross-source fusion uses rank.
2. A missing dependency, invalid configuration, unreachable source, or adapter
   crash is never reported as a successful empty result.
3. Retrieved content is data. It never changes the retrieval plan, adapter
   command, configuration, permission scope, or available tools.
4. Trust is assigned at Recall's boundary. An adapter cannot mark
   source-derived text as trusted and cannot lower its source's sensitivity
   floor.
5. Absence is proven only by a complete, successful source boundary. A record
   missing from a partial scan is unknown, not deleted.
6. Every configured value and every contract field affects an explained code
   path and has an end-to-end test. Dead configuration and dead fields are
   defects.
7. The core owns no index. Indexes belong to adapters and are rebuildable
   projections of a source Recall does not own.
8. Recall reconstructs nothing about the caller. Query text, conversational
   context, and suppression state arrive explicitly in the request.
9. Query text and evidence are not written to logs or analytics by default.
10. Deleting Recall's cache or state directory changes latency, never results.

## Terms

**Source**
: A logical retrieval authority with coherent query semantics, freshness rules,
permissions, and provenance. Not necessarily one file or database.

**Adapter**
: The implementation connecting Recall to a class of source. Owns discovery,
querying, health, indexing, and expansion for that class.

**Source instance**
: One configured use of an adapter, with its own identity, location, policy,
and ranking prior. Two projects may use one adapter without becoming one source.

**Profile**
: A named set of source instances and policies selectable per project, machine,
or query.

**Record**
: A native item exposed by a source: a document section, person, task, message,
event.

**Candidate**
: A compact search result returned by an adapter for fusion.

**Evidence**
: Expanded source material retrieved from a candidate's locator.

**Locator**
: A stable, printable reference that retrieves a record or evidence range
again. Only the owning adapter interprets its internal structure.

**Lineage root**
: The identity of the original record a candidate projects, after following
declared derivation edges. The deduplication unit.

**Cluster**
: A set of lineage groups referring to the same entity, event, or fact. The
display and corroboration unit.

**Retrieval plan**
: The eligible sources, per-source limits, filters, and budgets resolved for
one request.

## Architecture

Recall is a modular monolith with ports and adapters. Adapters know storage
engines. The ranking core knows only Recall contracts.

```text
Host
 |
 v
Query API  ──►  Plan: profile resolution, eligibility, budget allocation
                 |
                 +──► adapter ──┐
                 +──► adapter ──┤  concurrent, per-source deadline
                 +──► adapter ──┘
                                |
                                v
                 Source-local ranked candidate lists
                                |
                                v
                 Lineage grouping  (declared edges; dedup unit)
                                |
                                v
                 Cluster + corroborate  (entity/fact; display unit)
                                |
                                v
                 Score, order, exact-match promotion
                                |
                                v
                 Diversity selection, budget shaping
                                |
                                v
                 Candidates + locators + explanations
                                |
                                v
                 Expand (stateless, by locator)
```

Module boundaries:

- An adapter may improve its own retrieval without changing fusion.
- Fusion may change without learning any storage schema.
- Every product surface calls one application layer. A surface never acquires
  its own ranking, permission, or expansion behavior.

## Query Contract

### Request

```text
query                exact current request text
profile              named profile, or default resolution
scope                optional source, record-type, and time filters
as_of                optional historical boundary (RFC 3339)
context              optional bounded list of prior message texts
conversation_id      optional identity, for host correlation only
request_id           correlation identity for tracing and logs
suppress_lineages    lineage roots the host has already shown
mode                 explicit | pre_reply
budget               latency_ms and response_tokens
limit                maximum fused results
```

The host owns conversational state. Recall does not read transcripts or
session logs, and holds no per-conversation state. `request_id` is a
correlation identity, not an idempotency key: Recall stores no request results.

`context` entries are plain text used only for lexical query expansion inside
adapters that declare that capability. Weighted context is not in v1; add it
only with a code path that consumes the weight.

### Response

```text
results              ordered clusters, budget-shaped
source_outcomes      per-source outcome, latency, and freshness evidence
plan                 the resolved retrieval plan
suppressed           counts and reasons for withheld candidates
outcome              answered | abstained | failed
coverage             complete | degraded
truncated            bool, with dropped-result count
```

`outcome` and `coverage` are orthogonal. A run may abstain with complete
coverage, or answer with degraded coverage. Degraded coverage is stated
inline; Recall never silently narrows coverage.

Not every omission degrades coverage. A source left out because the request
scoped it away, because it is disabled, or because the profile ceiling sits
below its classification is the configured system working as asked. Reporting
that as degraded would make every well-configured query look impaired and the
signal would stop meaning anything. A source that was eligible and could not
answer — unhealthy, denied, out of budget, or unable to honor `as_of` —
degrades coverage. Both are reported in `source_outcomes` either way.

`truncated` means budget shaping dropped trailing results. Truncation is not
degradation.

Each result carries its primary candidate, cluster members with lineage roots,
a structured score explanation, and a locator. Leading results include
excerpts; trailing results compress to one line plus locator. The same
structure serializes to JSON and renders as tiered text. Neither surface gets
extra fields.

### Abstention

Recall abstains when nothing survived selection and at least one source
answered. It reports `failed` instead when every source that was asked failed:
"no results" is a claim about the corpus, and nothing supports it when nothing
looked.

Abstention is a rule over results and source outcomes, never a threshold on a
fusion score. Those scores are ordinal and uncalibrated, so a threshold on one
would be a number pretending to be a confidence. An earlier draft of this
document said Recall abstains when no cluster carries a "direct match signal" —
that would make every paraphrase query abstain in a lexical-first v1, since only
an identifier match is direct.

### Expand

```text
locator              from a prior result
detail               summary | excerpt | full | context
budget               hard output limit in bytes
```

Expansion is stateless with respect to the original query. A locator printed
yesterday expands today unless the source changed incompatibly, and that
failure is explicit. Expansion re-checks permissions.

### Budget Allocation

```text
deadline        = start + budget.latency_ms
fusion_reserve  = configured, default 25ms
source_deadline = min(source.timeout_ms, deadline - fusion_reserve - now)
```

A source whose deadline has already elapsed is skipped and reported; coverage
becomes degraded. Late results are discarded, never attached to a later
request. Timed-out sources report `timeout`, never empty success.

Response shaping is deterministic: assign excerpt budget greedily from the top
until `response_tokens` is exhausted, then emit one-line entries, then truncate
and set `truncated`.

### Invocation Modes

**explicit**
: A user or agent asks. Suppression never hides requested evidence.

**pre_reply**
: The host requests a small evidence budget before composing an answer, passing
the exact current user message. Pre-reply recall finishes inside its latency
budget or returns a visible degraded outcome. It never falls back to a previous
message.

Suppression filters passive display only, keyed on lineage root, using the
`suppress_lineages` list the host supplies. It never affects source retrieval
or explicit queries, and every suppressed candidate is counted with a reason.

## Identity, Locators, And Lineage

### Source Identity

Every source instance declares two identifiers:

- `source_uid` — immutable, generated once at configuration time, never
  edited. All persisted references key on this: evaluation judgments, saved
  locators, telemetry.
- `source_id` — human-readable display and CLI name. May be renamed freely.

Locator text uses `source_id` so locators stay readable
(`tasks:td-f62256`, `clara-docs:projects/recall/spec.md#ranking`). Persisted
references store `source_uid` plus the adapter-local part, so renaming a source
does not invalidate an evaluation pack. `recall config explain` prints the
mapping, and `recall doctor` fails on a duplicate or missing `source_uid`.

A locator is portable only within a configuration namespace that defines its
`source_uid`. Expanding a locator whose source is not configured on this
machine fails explicitly with `source_not_configured`.

### Lineage

Lineage identity is declared, never inferred:

1. **Record-level.** A candidate carries `derived_from` locators naming the
   upstream records it projects. Primary edge.
2. **Source-level.** A manifest declares `derives_from` when an entire source
   projects another. Used when record-level references are unavailable.
3. **Content fingerprint (advisory).** A normalized content hash collapses
   candidates for corroboration counting when no declared edge exists, so two
   sources holding the same text do not corroborate each other.

   It does that and nothing else. A fingerprint never clusters candidates for
   display, never selects a primary, and never suppresses one source's result
   as a duplicate of another's. Primary selection is by score and a top local
   rank is free to whoever is answering, so an advisory hash that merged across
   sources would let any source capture another's cluster and become the record
   a person is shown. Collapsing corroboration is safe in a way clustering is
   not: its only effect is a lower score.

The lineage root is the locator of the original record after following declared
edges, expressed with `source_uid`. Edges are followed to a fixed depth
(default 4).

A cycle among **source-level** `derives_from` edges is configuration, and
`recall doctor` reports it. A cycle among **record-level** `derived_from` edges
is not: those edges live in candidates, so nothing can enumerate them without
running a query over every corpus. Record-level cycles are detected per request
and reported in that response's diagnostics.

## Configuration And Trust

Configuration is TOML; comments are part of the contract. Machine-readable
output (`recall config explain`, `recall sources --json`) is JSON.

### Layers And Trust Boundary

Two layers, deterministic merge, project over user:

- **User configuration** (`$XDG_CONFIG_HOME/recall/config.toml` and
  `adapters.d/`) is the only place an adapter **command** may be declared. It is
  trusted.
- **Project configuration** (`recall.toml` in a project) may reference adapters
  by name and supply locations, scopes, priors, record types, timeouts, and
  adapter settings. It may never introduce an executable path, argv, or
  environment for a subprocess.

This boundary exists because a project configuration travels with a cloned
repository. Loading one must never be able to execute attacker-chosen code, and
must never let the repository redirect an existing source. The project layer
therefore also may not:

- Declare `[defaults]` at all. Selecting the active profile is the sharpest
  case: a project cannot raise a ceiling, but pointing `defaults.profile` at a
  more permissive profile reaches the same end without touching one.
- Choose a `source_uid`, for a new source or an existing one. A project may
  introduce a source — searching a repository's own documents is the point —
  but its identity is derived from the declaring file and the source name, not
  declared. A chosen uid can make a saved locator or an evaluation judgment
  resolve against repo-chosen data on a machine where the real source is
  absent.
- Change `location`, `settings`, or `enabled` on a source the user layer
  declared. Those decide what data answers under a trusted source's name, and
  `settings` is the sharper of them: it is adapter-owned and unvalidated at
  load, so a key like `cli` can name a program without ever looking like an
  executable key.
- Repoint a source at a different adapter.
- Replace a profile's source list. Project membership is additive; deciding
  what a trusted profile no longer contains would hide the authoritative source
  for a question and leave only the repository's own.
- Move a sensitivity floor or a profile ceiling in the permissive direction.
  Invariant 4 forbids an adapter lowering a floor; a project file is no more
  trusted than an adapter.

A project may tune what is bounded and reversible: priors, intent priors,
record types, timeouts, and its own sources' everything.

The catch-all profile exists only when no profile is configured at all. It has
no ceiling and every enabled source, so synthesizing it beside profiles a user
deliberately restricted would leave a permissive profile permanently reachable.

Executable keys are rejected by name anywhere in a project document, including
inside the adapter-owned `settings` table, which is otherwise the natural place
to hide one. Secret references use `env_var` rather than `env` so the scan can
stay blunt. `recall doctor` fails loudly on every rule above.

Recall never discovers and runs programs found in a source directory.

### Adapter Registration

A user-level adapter record declares what the core needs before any adapter
process exists. Configuration is validated at load time, when no manifest has
been exchanged, so the record must restate the parts validation depends on:

```text
command                 executable, user layer only
args, env               argv and environment, user layer only
freshness_modes         modes this adapter can serve
conformance             optional absolute directory of recorded transcripts,
                        replayed by `recall doctor --conformance <adapter>`
```

`conformance` is absolute for the same reason `command` is: a relative path
resolves against whatever directory Recall was started in, and a conformance
run that silently checked a different suite would be worse than one that
checked nothing. A built-in adapter may not declare one — there is no process
to replay a transcript against, and its suite runs in the Go test suite.

A manifest that contradicts its registration fails the handshake. A source that
omits `freshness_mode` inherits it only when the adapter supports exactly one.

### Source Instance

```text
source_uid          immutable identity, generated once
source_id           display and CLI name, unique within the profile
adapter             registered adapter name
location            path, endpoint, or connection reference
enabled             explicit on/off
record_types        optional scope narrower than the adapter default
freshness_mode      live | indexed | hybrid, must be supported by the adapter
base_prior          cross-source prior, validated range
intent_priors       bounded adjustments per named query class
freshness_policy    expected refresh or verification behavior
sensitivity         classification floor for this source
timeout_ms          per-source query budget
settings            adapter-owned, schema-validated
```

Relative paths in a project file resolve from that file. Secrets are references
to environment variables or an OS keychain and never appear in resolved output.

`location` has a syntax-only interpretation. A bare value such as an email
account, mailbox, device name, or database identifier is opaque and reaches the
adapter byte-for-byte. URI schemes (`https://...`, `google://...`, `urn:...`)
are also opaque. A value is a filesystem path only when it declares filesystem
syntax: `/`, `./`, `../`, `~`, a `/` or `\` separator, or a Windows drive/UNC
form. Explicit relative paths resolve from the file that declared them and `~`
expands to the current user's home. Recall never guesses from whether a file
happens to exist. Consequently, a bare project directory must be written
`./notes`, not `notes`. `recall config explain` reports the original value, the
resolved value, the classified kind, and whether rewriting occurred.

An `intent_priors` entry is the prior **in force** for its query class, not a
delta applied to `base_prior`. Both are validated against the same `[0.5, 2.0]`
range, which is what makes them comparable and bounded; a delta sharing that
range would be a different quantity wearing the same units. The explanation
reports `intent` as the derived difference, so a reader sees what the rule
changed as well as where it landed.

Priors appear in every score explanation and are evaluation parameters. A prior
expresses expected authority for a query class. It does not calibrate a
source's native score. Configuration must not become an unbounded scoring
language.

### Permissions

v1 has no per-record ACLs. Access is source-level:

- Sensitivity is an ordered scale: `public < internal < confidential <
  restricted`.
- A profile declares `max_sensitivity`. Sources above it are ineligible, and are
  reported as `skipped` with reason `sensitivity_ceiling` — not as `denied`,
  which means the source itself refused. Honoring a configured ceiling is the
  system working as asked, so it does not degrade coverage either.
- A candidate may raise its sensitivity above the source floor, never lower it.
  A candidate raised above the profile ceiling is dropped and counted in
  `suppressed` with reason `sensitivity`.

Permissions are enforced before retrieval and again before expansion. A
configured source may be unavailable or forbidden on another machine.

## Sources

### What Counts As A Source

Source identity follows retrieval semantics, not filesystem boundaries.

- A directory of notes under one indexing and privacy rule is one source; files
  are records.
- One SQLite database may expose several logical sources when contacts, events,
  and messages have different relevance and freshness semantics.
- Several JSONL files sharing a schema and retention policy may form one
  source; one JSONL file with unrelated event families may need several.
- A remote API is one source only when its records share a query contract and
  permission boundary.

This is what makes a source prior meaningful. A weight on "SQLite" means
nothing; a prior for "work items" can be evaluated.

### Source Classes

**Document corpora** — Markdown, notes, documentation, generated summaries.
Chunk-aware lexical search, optional semantic search, file and heading
metadata, direct reads by path and range. Usually needs an adapter-owned index;
original files remain the source of truth.

**Structured databases** — people, tasks, projects, events. Exact identifier
lookup, field filters, native FTS, adapter-defined structured queries. Typed
fields are preserved in candidate metadata; flattening a person or task into
anonymous text discards ranking and routing signal. Read-only access.

**Append-only streams** — JSONL event logs, traces, message archives, audit
streams. Incremental ingestion from a cursor, schema-version-aware parsing,
time-window filtering, exact correlation lookup, and grouping adjacent events
into episodes while preserving per-record locators.

**Remote or computed** — same candidate contract, with explicit latency,
permission, rate-limit, and freshness reporting.

### Freshness Modes

**live**
: Query the source of truth per request. Small structured databases, exact
lookups, fast-changing state.

**indexed**
: Query an adapter-owned projection. Document corpora, large history, semantic
search, expensive remote systems.

**hybrid**
: Broad index plus live verification of recent or top records.

Every result reports which revision was searched. A healthy index can be stale.

### Index Obligations

The core owns no index. An adapter that maintains one must:

- Treat it as a rebuildable projection, never the source of truth.
- Build into a new generation and publish atomically, only after the declared
  source boundary completes. A failed build leaves the previous generation
  readable and marked stale.
- Checkpoint only after records and errors are durable, retaining the last
  successful watermark when a later pass fails.
- Record the embedding model identifier and version in the generation. A model
  change starts a new generation; one generation never mixes models. Queries
  report the generation's model as freshness evidence.
- Exclude records proven deleted upstream from the next published generation,
  and drop superseded generations on publication rather than keeping browsable
  history. Expansion caches honor the same boundary.

An adapter that maintains an index declares the `checkpoint` capability and
serves `recall/refresh`, which is the contract's only entry point for building
one outside the handshake. It also reports `index_config`, identifying the
retrieval configuration a generation was built under, so a scoring change
cannot alter results with nothing recording it.

Adapters receive a writable `workdir` at handshake for index storage. Recall
never resurfaces a record its source no longer contains, except through a
source whose contract retains history, such as an archive stream.

## Ranking

### 1. Eligibility

Eligibility uses hard constraints only:

- Explicit scope from the request.
- Permission and sensitivity policy.
- Adapter health and remaining latency budget.
- `as_of` support when the request carries `as_of`.
- Declared record-type scope versus requested record types.

There is no soft intent router and no identifier-based routing. Broad parallel
search is the default; exactness is handled at fusion, so the core never learns
which source owns which identifier format. Add routing only when evaluation
shows a measured cost.

### 2. Source-Local Retrieval

Each eligible adapter returns its best `k` candidates in local order, under a
per-source limit that prevents one large corpus from flooding the pool. Local
rank is the only mandatory relevance signal. `local_score` is diagnostic and is
never compared across sources.

### 3. Lineage Grouping

Group candidates by lineage root. Within a group keep the best `local_rank` per
source and select a primary candidate: highest source prior, ties broken by
locator sort for determinism.

```text
lineage_score(g) = max over sources s in g of
    prior(query, s) * 1 / (rank_constant + best_local_rank(s, g))
```

Maximum, not sum: the same record seen twice is not more evidence.

### 4. Clustering And Corroboration

Merge lineage groups referring to the same entity, event, or fact — stable
identifiers first, conservative matching otherwise. Entity matching never uses
unbounded substring tests; adapters supply exact identifiers, token-boundary
matches, aliases, or typed resolvers, and test known false positives.

Corroboration counts **units**, not lineage groups. Distinct lineage roots are
not sufficient evidence of independence: two chunks of one document and two
fingerprint-identical records have distinct roots but are one thing said once.
Groups collapse into one unit when they share `source_uid` plus
`source_record_id`, or `record_type` plus `content_fingerprint`. A unit's score
is the maximum of its groups'.

```text
cluster_score = min(
    sum over independent units u of unit_score(u),
    corroboration_cap * max unit_score(u)
)
```

Independence is directional — a composite depends on its parents — so the sum
is built by offering units in a defined order and accepting one only when it
adds independent evidence. Non-derivative units are offered first: a unit that
restates others (a composite with ancestors, or a source declaring
`derives_from`) is offered last regardless of score, so a strong summary is
never counted as the evidence while the records it summarizes are rejected as
restatements of it.

Defaults, all evaluation parameters: `rank_constant = 60`,
`corroboration_cap = 2.0` (never below 1, which would make corroborated
evidence score below a single group), priors in `[0.5, 2.0]`.

### 5. Exact-Match Promotion

Clusters containing a candidate with an `exact_identifier` match signal sort
above all clusters without one, ordered among themselves by `cluster_score`.
This is a deterministic partition, not a score bonus.

### 6. Optional Shared-Scale Reranking

A reranker may score the fused pool against the query on one shared scale. It
is not required for a useful v1, adds latency, and can mask routing errors. The
pre-rerank ordering and score breakdown are always retained and reported.

### 7. Diversity And Output

Final selection suppresses near-duplicates and preserves source diversity.
Diversity is a selection policy applied after relevance, not a substitute for
it. Output distinguishes high-confidence direct matches, related evidence,
source failures or missing coverage, and no supported recall.

## Explainability

Every result carries a structured explanation, not a rendered string. Text
output is derived from it, and evaluation gates assert on its fields.

```text
source_uid, source_id
local_rank, local_pool_size
match_signals            exact_identifier | lexical | semantic | field | alias
prior_applied            base and intent components, with the rule that fired
lineage_root
corroboration            count of distinct lineage roots, contributing sources
freshness                mode, source_revision, index generation, model
reranker                 used | not used, with delta
suppressions             reasons, if any
```

Every configured value that affected the result appears here. A setting that
cannot appear in an explanation does not exist.

## Time

Recall distinguishes four timestamps and never collapses them into `last_seen`:

**event_time**
: When the underlying event happened.

**valid_from / valid_to**
: When a fact was or is true. A newer record may supersede an older one without
making the older irrelevant to a historical query.

**observed_at**
: When Recall read or indexed the record. Observation alone proves nothing
about a later source boundary.

**confirmed_at**
: When a complete successful source operation confirmed the record present or
current.

Reinforcement time exists only inside memory-owning sources, behind their
adapters.

### `as_of`

Adapters declare `as_of_support`:

```text
none      cannot answer historical queries
filter    can restrict by event or validity time it already stores
snapshot  can reconstruct source state at a past instant
```

A request carrying `as_of` excludes sources declaring `none` and reports them
in `source_outcomes` with reason `as_of_unsupported`; coverage becomes
degraded. A source never answers an `as_of` query from current state.

### Decay

The core never implements decay. Decay, reinforcement, and archival are
memory-store semantics requiring record ownership, and Recall owns no records.
An adapter whose source has native decay applies it inside source-local
ranking; fusion consumes local rank, so decayed weights never reach
cross-source comparison. This delegation is permanent.

An adapter adding decay to a source without native support must follow these
rules:

- Select decay by record semantics, not storage type.
- Durable facts do not become false with age. Only transient observations and
  inferred preferences lose influence.
- Reinforcement requires new evidence or explicit behavior. Retrieval hits and
  re-observation of the same assertion never reinforce.
- Faded material stays recoverable and searchable for historical questions.
- Half-lives and archive thresholds are inspectable policy tuned from
  evaluation evidence.
- A historical query retrieves old evidence without a recency penalty.
  Contradicting evidence changes validity rather than applying negative decay.

## Security And Privacy

- Treat all retrieved content as untrusted data, never as instructions.
- Strip unsafe control characters, bound text field lengths, and validate links
  before candidates reach a terminal, API, MCP client, or model.
- Open databases read-only by default.
- Adapter commands come only from user configuration (see Trust Boundary).
- Do not log full queries or excerpts by default.
- Carry provenance and source revision through every transformation.
- Redact diagnostics so a denied source does not leak record existence.
- Keep adapter secrets out of manifests, result envelopes, and resolved config.
- Remote embedding or reranking is an explicit, configured data-egress
  decision, reported in freshness evidence.

## Observability

Recorded without query content by default:

- Eligible, searched, skipped, degraded, denied, and failed sources.
- Per-adapter and total latency; adapter cold-start time.
- Candidate counts before and after grouping, clustering, and selection.
- Expansion success rate and failure reasons.
- Source and index watermarks; fallback paths used.
- Reranker usage and cost.
- Suppressed candidate counts and reasons.
- Host-reported selection or citation events.

Candidate- and locator-level telemetry is opt-in: identifiers reveal subjects.
Opt-in evaluation logs retain query text and judgments in a separate, clearly
classified dataset.

## Process Model And State

### Adapter Processes

External adapters are long-lived JSON-RPC servers, spawned on first use and
pooled per source instance for the process lifetime. Health is probed on spawn
and cached with a TTL (default 30s). Cold-start time counts against the
request's latency budget and is reported separately in diagnostics.

Under the per-query CLI model every query pays cold start for every source.
`recall serve` exists to amortize it; the CLI may run in-process or dispatch to
a configured local service, and results must be identical either way.

### State Layout

```text
$XDG_CONFIG_HOME/recall/config.toml     user configuration
$XDG_CONFIG_HOME/recall/adapters.d/     registered adapter definitions
$XDG_STATE_HOME/recall/<profile>/       adapter workdirs, indexes
$XDG_CACHE_HOME/recall/<profile>/       expansion and health caches
```

Deleting cache or state changes latency, never results. Evaluation run
artifacts default to the state directory and are never committed.

## Surfaces

All surfaces call one application layer.

### CLI

Canonical operator surface. Every read command supports `--json` alongside
concise human output.

```text
recall query      search and fuse configured sources
recall expand     retrieve evidence from a locator
recall sources    list source instances and capabilities
recall config     explain resolved configuration and its origins
recall doctor     validate config, trust boundary, access, health, freshness
recall eval       run a versioned retrieval benchmark
recall serve      run the local HTTP API
recall mcp        run the MCP server
```

### Local HTTP API

Versioned, loopback-only by default. Non-loopback access requires explicit
configuration and authentication. Justified by long-lived indexes, shared model
processes, concurrent hosts, and clients that should not spawn a process per
query.

### MCP

An adapter from agent hosts to Recall, not an internal boundary. Exposes query,
expand, and source-status tools, preserving locators and diagnostics rather
than flattening results into unstructured text. Recall may separately consume
an upstream MCP server through a source adapter; the two roles are independent.

## Implementation

The core is one Go module: CLI, configuration loader, source orchestrator,
fusion engine, expansion, and evaluation runner. Go fits the shape — a single
dependable executable, cancellable concurrent I/O, subprocess supervision, and
a Tier 1 official MCP SDK. Local ML stays out of the core; embedding and
reranking run behind adapters.

Go shared-library plugins are not used. Extensibility is built-in adapters plus
out-of-process adapters over the versioned protocol.

### Package Boundaries

```text
cmd/recall           CLI entry points
internal/recall      domain types; depends on nothing else
internal/lineage     locator resolution, lineage roots, independence
internal/config      loading, merge, trust boundary, validation
internal/source      registry, instance resolution, eligibility, plan
internal/app         orchestration: fan out, admit, fuse, shape, decide
internal/adapter     adapter interface, subprocess supervision
internal/protocol    JSON-RPC framing, schemas, conformance replay
internal/ranking     grouping, clustering, fusion, selection
internal/evidence    expansion, budget shaping, sanitization
internal/explain     structured explanations and rendering
internal/eval        packs, metrics, gates, runs, reports
internal/api         HTTP and MCP transports
```

Built-in and external adapters implement the same Go interface. JSON-RPC is a
transport over that interface, not a second contract.

### First Vertical Slice

Two heterogeneous sources prove the architecture:

1. **Documents** — built-in, indexed, lexical only. Clara project Markdown.
2. **Tasks** — built-in, live, structured, through the Tasks CLI JSON contract.

Plus one **reference external adapter** shipped as a fixture: a small stream
adapter that emits `derived_from` edges, exercising the wire protocol and the
lineage path with real code rather than fixtures alone.

Sequence after the slice: Clara signals (first real lineage and temporal
source), then td workspaces, then semantic document retrieval, then optional
reranking. Each layer earns its place in evaluation before the next begins.

### Replaceable Engines

Recall owns the source registry and adapter contract, eligibility, fusion,
lineage, freshness, permissions, provenance, expansion, temporal semantics, and
evaluation. Specialized engines sit behind adapters when they solve a hard
subproblem better.

[QMD](https://github.com/tobi/qmd) is the intended second document backend:
local BM25, vector search, hybrid fusion, local reranking, bounded line
retrieval, JSON output. It ships **after** the built-in lexical adapter, as a
second implementation of the same adapter contract, so the benchmark can
compare them. It integrates over a process boundary and never dictates the core
language.

[MemoryWiki](https://github.com/MemoryWiki/MemoryWiki) is a design reference
for provenance, conflict preservation, and write safety when memory
construction is in scope. Not a dependency.

## Open Decisions

1. Which source priors are justified for the first query classes?
2. When is shared-scale reranking worth its latency and data-egress cost?
3. What query and result data may be retained for evaluation?
4. Which host integration follows the CLI: library, local service, or MCP?
5. Does the Go slice satisfy the acceptance checks below?

### Language Acceptance Checks

The Go decision holds once the slice proves:

1. A built-in and an external source are searched concurrently.
2. CLI and MCP return the same typed result.
3. Cancellation terminates a slow external adapter.
4. Cross-platform builds produce clean binaries with no hidden runtime
   dependencies.
5. The benchmark runner executes deterministic fixture cases.

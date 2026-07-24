# Recall Specification

**Status:** Working draft

**Last updated:** 2026-07-23

Recall is a portable retrieval subsystem for personal AI agents. It searches heterogeneous, user-controlled data and returns compact, source-grounded evidence that an agent can inspect or expand.

The first design problem is source integration. Recall must search text documents, SQLite databases, JSONL streams, and future adapters without pretending that their native relevance scores mean the same thing.

## Product Context

Recall is intended for a new personal agent system that should be more portable than OpenClaw and suitable for work environments. It may run inside different agent hosts over time, so its core contracts must not depend on one runtime, programming language, model provider, or storage engine.

The expected deployment is local-first and single-user at the beginning. Some sources may later be remote or shared. Work data makes provenance, permissions, and query privacy part of the base design.

Recall is a standalone product rather than a Clara subsystem. Clara, Tasks, td,
and the first personal agent are initial consumers and source examples. None of
their schemas, directory layouts, or ranking assumptions belong in the Recall
core.

## Goals

- Search multiple logical sources through one stable interface.
- Work across unrelated projects through declarative configuration.
- Preserve source-native search behavior instead of forcing every source into one index.
- Combine source-local result lists without comparing meaningless raw scores.
- Return compact evidence first, with progressive expansion when the agent needs more.
- Keep every result traceable to a stable source locator.
- Make missing, stale, denied, and unhealthy sources visible.
- Support both live retrieval and indexed projections.
- Allow new adapters without changes to the ranking core.
- Expose the same behavior through a CLI and optional local API and MCP server.
- Measure retrieval quality using real questions and downstream answer utility.

## Non-Goals For The First Version

- Choosing what deserves to become durable memory.
- Editing or synchronizing source data.
- Building a universal knowledge graph.
- Replacing the agent runtime or its conversation manager.
- Training a global ranking model before a useful labeled dataset exists.
- Converting every source into a single canonical storage schema.
- Becoming a general data synchronization or ETL platform.
- Requiring adapters to share Recall's implementation language.

Recall may later participate in memory capture or consolidation, but retrieval should stand on its own first.

## Design Lineage

Recall keeps the strongest ideas from Scent and Clara while changing their
product boundary. These are design inputs, not compatibility requirements.

### Scent Ideas To Preserve

| Scent idea | Recall commitment |
| --- | --- |
| Retrieve before the reply when continuity matters | Hosts may invoke Recall as a pre-reply step using the exact current request. Passive recall remains a host policy, not a hidden global hook. |
| Pointers before payloads | Queries return compact candidates with stable locators; expansion retrieves stronger evidence under a budget. |
| Search each kind of data appropriately | Exact identifiers, structured filters, lexical search, semantic search, and temporal lookup remain source-local capabilities. |
| Keep the cheap path cheap | Exact and lexical retrieval form the baseline. Semantic expansion and reranking must earn their latency in evaluation. |
| Avoid repeating the same context | Optional conversation-scoped suppression prevents passive recall from injecting the same evidence lineage on every turn. |
| Reinforce agreement across sources | Independent evidence can increase confidence after candidates are clustered by lineage. Duplicate projections do not count twice. |
| Make retrieval inspectable | Local data, typed results, provenance, score explanations, and stable evidence references remain default design choices. |
| Measure actual use | Recall measures expansion and host-reported consumption, not only query count and latency. |

### Scent Failure Patterns To Reject

- Raw scores from different systems are never normalized into a pretend common
  scale.
- Missing dependencies, invalid configuration, inaccessible databases, and
  adapter failures never become successful empty result sets.
- A host passes the current request directly. Recall does not reconstruct it by
  scraping a session log or depend on a private host patch.
- Partial indexes, stale embeddings, and truncated source scans are reported as
  degraded. A recent index timestamp alone is not health.
- Recall does not build a second document or embedding stack when a replaceable
  engine such as QMD performs that job better.
- Entity matching cannot rely on unbounded substring tests. Adapters use exact
  identifiers, token boundaries, aliases, or typed resolvers and test common
  false positives.
- Query text and evidence are not written to plaintext analytics by default.
- Every configured ranking or decay value must affect an explained code path
  and have an end-to-end test. Dead configuration is a defect.

### Clara Ideas To Preserve

| Clara idea | Recall commitment |
| --- | --- |
| Deterministic core with an agentic edge | Parsing, identity, policy, fusion, and output contracts are deterministic. External tools and model-backed retrieval stay behind adapters. |
| Stable, versioned records | Adapter messages and Recall-owned indexes use explicit schema versions, stable source identities, and provenance. |
| Source completion is evidence | Absence is meaningful only after a complete successful source boundary. Partial or failed scans cannot retire records or prove no match. |
| Separate kinds of memory | Durable facts, inferred preferences, transient signals, and behavioral observations have different temporal policy. |
| Forget by semantics | Decay applies only to suitable record classes. Faded material moves to recoverable cold storage rather than disappearing. |
| Reinforce from evidence | New evidence or explicit behavior may reinforce an inference. Search and retrieval hits never do. |
| Preserve behavioral truth | If Recall later learns preferences, append-only observations remain separate from the derived, rebuildable preference projection. |
| Treat source content as hostile input | Source data cannot promote its own trust, alter the retrieval plan, or authorize a tool call. |

### Clara Limits Not To Inherit

- Clara's current memory recall is substring filtering followed by decayed
  weight. Recall needs exact, lexical, semantic, structured, and temporal
  retrieval plus cross-source fusion.
- Clara's default half-lives are evidence from one application, not defaults for
  Recall.
- Clara-specific schemas and work routines remain optional adapters or profiles.
  They never enter the core.
- One timestamp cannot stand for indexing, source confirmation, event time, and
  semantic reinforcement.

## Terms

**Source**
: A logical retrieval authority with coherent query semantics, freshness rules, permissions, and provenance. A source is not necessarily a physical file or database.

**Adapter**
: The implementation that connects Recall to a source. It owns source-specific discovery, querying, health checks, and expansion.

**Source instance**
: One configured use of an adapter, with its own identity, location, policy,
permissions, and ranking prior. Two projects can use the same adapter without
becoming the same source.

**Profile**
: A named set of source instances and policies that can be selected for a
project, machine, or query.

**Record**
: A native item exposed by a source, such as a document section, person, task, message, or event.

**Candidate**
: A compact search result returned by an adapter for cross-source fusion.

**Evidence**
: Expanded source material retrieved from a candidate's locator.

**Locator**
: A stable, printable reference that lets the adapter retrieve a record or
evidence range again. Locators are readable and copy-pasteable, such as
`tasks:td-f62256` or `clara-docs:projects/recall/spec.md#ranking`. Only the
owning adapter interprets a locator's internal structure; other components
treat it as an identifier.

**Evidence lineage**
: The identity that groups candidates derived from the same original record, so
projections of one fact are not mistaken for independent corroboration.

**Retrieval plan**
: The set of eligible sources, per-source limits, filters, and query variants used for one request.

**Fusion**
: Combining ranked candidate lists from multiple sources into one result set.

## Architectural Boundary

Recall should begin as a modular monolith with ports and adapters. Adapters know storage engines. The ranking core knows only the Recall contracts.

```text
Agent or host
     |
     v
Recall query API
     |
     v
Query interpretation and source eligibility
     |
     +----------+-----------+-----------+----------+
     |          |           |           |          |
     v          v           v           v          v
Documents    Contacts     Tasks      Events     Future source
adapter      adapter      adapter    adapter    adapter
     |          |           |           |          |
     +----------+-----------+-----------+----------+
                            |
                            v
                  Ranked source-local lists
                            |
                            v
                Fusion, clustering, and diversity
                            |
                            v
                Optional shared-scale reranking
                            |
                            v
               Compact candidates with locators
                            |
                            v
                   Progressive evidence fetch
```

The source adapters and the cross-source ranker are separate modules. An adapter may improve its own retrieval without changing fusion behavior. The fusion strategy may change without teaching it SQLite schemas or Markdown chunking.

## Query Contract

The query contract is the host-facing API. The CLI, local API, and MCP surface
are thin transports over this one contract. Scent's `smell`/`hunt` CLI validated
this two-verb shape in daily use; the contract below generalizes it.

### Query Request

```text
query                 the exact current request text
profile               named profile, or default resolution
scope                 optional source, record-type, and time filters
context               optional bounded prior messages with explicit weights
conversation_id       optional identity for suppression scope
request_id            unique identity for tracing and idempotency
mode                  explicit or pre_reply
budget                latency budget and response token budget
min_confidence        optional abstention threshold
limit                 maximum fused results
```

Context is always passed explicitly by the host with per-message weights.
Recall never reconstructs conversational context from transcripts or session
logs.

### Query Response

```text
results               fused, clustered candidates in final order
source_outcomes       per-source outcome, latency, and freshness evidence
plan                  the resolved retrieval plan that produced this response
suppressed            count and reasons for suppressed candidates
outcome               answered, degraded, abstained, or failed
```

Each result carries its candidate envelope, cluster members with lineage,
score explanation, and locator. The response is budget-shaped: leading results
include excerpts, trailing results compress to one line with a locator the
caller can expand. The same structure serializes to JSON for machine callers
and renders tiered text for humans; neither surface gets extra fields.

Degraded is a first-class outcome. A response that omitted an eligible source
says so inline; it never silently narrows coverage.

### Expand Request

```text
locator               from a prior result
detail                summary, excerpt, full record, surrounding context
budget                hard output limit
```

Expansion is stateless with respect to the original query. A locator printed
yesterday should still expand today unless the source changed incompatibly, and
that failure is explicit.

## Evidence Identity And Lineage

Lineage determines clustering, corroboration, suppression, and evaluation
judgments. Identity is declared, not inferred:

1. **Record-level derivation.** A candidate may carry `derived_from` locators
   naming the upstream records it projects. This is the primary lineage edge.
   A Clara signal that projects a Tasks record declares that Tasks locator.
2. **Source-level derivation.** A manifest may declare `derives_from` when an
   entire source is a projection of another source. Used when record-level
   references are unavailable.
3. **Content fingerprint (advisory).** A normalized content hash may collapse
   candidates for corroboration counting when no declared edge exists. A
   fingerprint match is never identity: it can merge duplicates, but two
   fingerprint-merged candidates still count as one lineage for corroboration
   and remain separate records for expansion.

A lineage identifier is the locator of the root record after following declared
derivation edges. Candidates sharing a lineage root cluster together and count
once for corroboration. Independent corroboration requires distinct lineage
roots.

## Portable Configuration

Recall should load source instances and ranking policy from configuration. The
core must not contain a built-in list of a user's repositories or databases.

Configuration files use TOML: they are hand-edited, and comments are part of
the contract. Machine-generated output such as `recall config explain` emits
JSON.

A source instance needs configuration equivalent to:

```text
source_id              stable identity within the configuration namespace
adapter                adapter type and compatible protocol version
location               path, command, endpoint, or connection reference
enabled                 explicit on/off state
record_types            optional scope narrower than the adapter default
retrieval_mode          live, indexed, or hybrid
base_prior              modest cross-source prior
intent_priors           bounded adjustments for named query classes
freshness_policy        expected refresh or verification behavior
sensitivity             default data classification
timeout                 per-source query budget
settings                adapter-owned, schema-validated configuration
```

Configuration should support two layers:

- User configuration contains machine-wide adapters and reusable profiles.
- Project configuration selects or adds sources for one project.

The merge order must be deterministic and explainable. `recall config explain`
should show the resolved profile, the origin of each value, and validation
errors without printing secrets.

Relative paths in project configuration resolve from that configuration file.
Secrets are references to environment variables, an operating-system keychain,
or another secret provider; they are never copied into the resolved config
output.

Configuration can tune source priors, but it must not turn ranking into an
unbounded scoring language. Priors and intent adjustments use validated ranges,
appear in result explanations, and remain benchmark parameters. A source weight
expresses expected authority for a query class. It does not calibrate the
source's native search score.

## Product Surfaces

All product surfaces call the same application layer. They must not acquire
separate ranking, permission, or expansion behavior.

### CLI

The CLI is the first and canonical operator surface. Every read command has
stable structured output in addition to concise human output.

The initial command shape should cover:

```text
recall query             search and fuse configured sources
recall expand            retrieve evidence from a stable locator
recall sources           list source instances and capabilities
recall doctor            validate config, access, health, and freshness
recall eval              run a versioned retrieval benchmark
recall serve             run the local HTTP API
recall mcp               run the MCP server
```

Command names remain provisional until the query and evidence contracts are
written.

### Local API

A versioned HTTP API may expose query, expansion, source status, and health when
a long-lived service has a concrete consumer. It binds to loopback by default.
Non-loopback access requires explicit configuration and authentication.

The API is useful for long-lived indexes, shared model processes, concurrent
agent hosts, and clients that should not spawn a process per query. The CLI may
run the application in-process or call a configured local service, but the
results must be equivalent.

### MCP

MCP is the likely second surface after the CLI. It is an adapter from agent
hosts to Recall, not Recall's internal module boundary. A small server can
expose query, expansion, and source-status tools. It should preserve Recall
locators and diagnostics rather than flattening every result into an
unstructured text response.

Recall may also consume an upstream MCP server through a source adapter when
that is the best supported interface. The two roles are independent.

### Invocation Policy

A host can use Recall in two modes:

**Explicit**
: A user or agent calls Recall for a query. Suppression does not hide requested
evidence.

**Pre-reply**
: The host requests a small evidence budget before composing an answer. The
request includes the exact current user message, conversation identity, request
identity, allowed profile, and privacy scope.

Pre-reply recall must finish inside a configured latency budget or return a
visible degraded outcome. It cannot fall back to the previous message. A late
result may be discarded, but it cannot silently attach to a later turn.

Conversation-scoped suppression keys on evidence lineage rather than rendered
text. Suppression affects passive display, not source retrieval or explicit
queries. It expires by policy and is explainable so a host can say why a result
was omitted.

## Adapter Extensibility

Recall should support two adapter forms:

**Built-in adapters**
: Compiled with Recall for common, low-level sources such as files, JSONL, and
read-only SQLite. These provide the simplest installation and strongest type
checking.

**External adapters**
: Executables or services that implement a versioned Recall adapter protocol.
They can be written in any language and can wrap an existing CLI, API, MCP
server, or proprietary SDK.

The external protocol should cover manifest, health, search, expansion, and
clean cancellation. A local subprocess transport is enough for the first
version; a network transport can reuse the same messages later. Recall should
not load native shared-library plugins.

Adapters are registered explicitly. Recall never discovers and executes
arbitrary programs found in a source directory.

## What Counts As A Source

Source identity should follow retrieval semantics, not filesystem boundaries.

Examples:

- A directory of Markdown notes governed by the same indexing and privacy rules can be one source. Individual files are records within it.
- One SQLite database may expose several logical sources. Contacts, contact events, and messages have different notions of relevance and freshness even if they share a database file.
- Several JSONL files with the same event schema and retention policy may form one source.
- One JSONL file containing unrelated event families may need separate logical sources or source partitions.
- A remote API is one source only when its records share a coherent query contract and permission boundary.

This definition gives source weighting something stable to mean. A weight on "SQLite" is meaningless. A query-dependent prior for "work items" or "people directory" can be understood and evaluated.

## Source Classes

Recall should support different retrieval modes rather than reducing every source to a document corpus.

### Document Corpora

Typical inputs:

- Markdown and text files
- Exported notes
- Project documentation
- Generated summaries

Likely retrieval:

- Chunk-aware lexical search
- Semantic search when available
- File and heading metadata
- Direct reads by path and range

Documents usually need a Recall-owned index or an existing search sidecar. The original files remain the source of truth.

### Structured Databases

Typical inputs:

- People and organizations
- Tasks and work items
- Projects
- Events and activity records

Likely retrieval:

- Exact identifier lookup
- Field-aware filters
- Native FTS when present
- Adapter-defined structured queries
- Optional projection into a search index

Structured sources should preserve typed fields in candidate metadata. Flattening a person, task, and event into anonymous text too early throws away useful ranking and routing signals.

Adapters should use read-only database access unless mutation is explicitly added as a separate capability.

### Append-Only Streams

Typical inputs:

- JSONL event logs
- Agent traces
- Message archives
- Audit or activity streams

Likely retrieval:

- Incremental ingestion from a byte offset or record cursor
- Schema-version-aware parsing
- Time-window filtering
- Event grouping into higher-value episodes
- Exact correlation or entity lookup

A raw event is not always a useful recall unit. The adapter may need to group adjacent events into an episode while preserving locators for the original records.

### Remote Or Computed Sources

Future adapters may query APIs, search services, or computed views. They use the same candidate contract but must report latency, permission failures, rate limits, and freshness explicitly.

## Source Adapter Contract

The following contract is conceptual. Concrete types and transport remain language agnostic.

### Manifest

Every source exposes a manifest:

```text
source_id             stable logical source identifier
adapter_id            adapter implementation and version
display_name          human-readable source name
record_types          person, task, document, message, event, ...
retrieval_modes       exact, lexical, semantic, structured, temporal
consistency_mode      live, indexed, or hybrid
derives_from          optional source_id this source projects
freshness_policy      how current results are expected to be
sensitivity           default data classification
capabilities          search, expand, enumerate, checkpoint
```

`source_id` identifies the logical authority. It must remain stable if a database file moves or an index is rebuilt.

### Health

An adapter reports:

```text
status                healthy, degraded, unavailable, denied
checked_at            time of the probe
last_success_at        latest complete successful operation
source_watermark      latest source revision, timestamp, or cursor
index_watermark       indexed revision when applicable
record_count          exact or estimated
indexed_count         records represented by the current index
failed_count          records rejected or not indexed
coverage              complete, partial, or unknown
diagnostics           safe operational details
```

An unavailable source must not look like a successful search with zero matches.
A partial source or index must not report healthy unless its declared policy
allows that exact partial boundary.

### Search

Input:

```text
query                 original user query
query_variants        optional lexical or semantic variants
filters               time, record type, entity, project, source scope
limit                 maximum candidates requested from this source
context               bounded conversational context when allowed
budget                latency or cost budget
```

Output:

```text
candidates            ranked source-local candidates
diagnostics           timing, query mode, fallback use, truncation
source_watermark      freshness evidence for this search
outcome               success, partial, unavailable, denied, or failed
```

The adapter owns local retrieval and ordering. It may use SQL, FTS, vector search, exact lookup, an API, or a combination.

### Candidate Envelope

Every candidate includes:

```text
candidate_id          stable within the source revision
source_id             logical source identifier
source_record_id       stable native or adapter-defined record identity
locator               stable printable expansion reference
derived_from          optional upstream locators for lineage
record_type           person, task, document, message, event, ...
title                 compact human-readable label
excerpt               bounded evidence preview
local_rank            mandatory rank in the source result list
local_score           optional source-native score
match_signals         exact, lexical, semantic, field match, filters
observed_at           when Recall observed this record
confirmed_at          when a complete source boundary last confirmed it
event_time            when the source event happened, if applicable
valid_from            optional fact validity start
valid_to              optional fact validity end
source_revision       revision or watermark used for retrieval
sensitivity           candidate-level classification
metadata              small typed fields useful for routing and display
```

`local_score` is diagnostic. Fusion must not assume that it is calibrated or comparable with another source's score.

### Expand

Expansion accepts a locator and a budget:

```text
locator               candidate reference
detail                summary, excerpt, full record, surrounding context
token_or_byte_budget   hard output limit
```

The adapter returns evidence with the source revision, provenance, and any truncation markers. Expansion should fail clearly when a locator has expired or the source changed incompatibly.

## Live, Indexed, And Hybrid Sources

Recall should not impose one freshness strategy on every adapter.

**Live**
: Query the source of truth at request time. Best for small structured databases, exact lookups, and rapidly changing state.

**Indexed**
: Query a Recall-owned or external projection. Best for document corpora, large JSONL history, semantic search, and expensive remote systems.

**Hybrid**
: Combine a broad index with live verification or recent records. Best when the full corpus is expensive to search but the latest state matters.

Each result must say which revision was searched. A healthy index can still be stale.

### Index Publication And Absence

Recall-owned indexes are rebuildable projections, never the source of truth.
An index build writes a new generation and publishes it atomically only after
the declared source boundary completes. Failed builds leave the previous
generation readable and mark it stale or degraded.

Incremental adapters checkpoint only after their records and errors are
durable. They retain the last successful watermark when a later pass fails.

A record missing from an incomplete scan is unknown, not deleted. An adapter
may mark a record absent, inactive, or superseded only when its source contract
defines the boundary that proves absence. Historical evidence stays
expandable whenever the source permits it.

An index generation records the embedding model identifier and version used to
build it. A model change starts a new generation; one generation never mixes
embeddings from different models. Queries report the generation's model as part
of freshness evidence.

### Deletion And Retention

Recall does not own source data, but it holds projections of it. When a source
boundary proves a record was deleted upstream, the next published index
generation excludes it, and superseded generations containing it are removed on
publication rather than kept as browsable history. Cached excerpts and
expansion caches honor the same boundary.

Recall never resurfaces a record whose source no longer contains it, except
through a source that itself retains history as part of its contract, such as
an archive stream. Evaluation packs store locators rather than copied bodies
for private data, so deleting the source record also removes it from future
benchmark runs.

## Cross-Source Ranking

### The Core Rule

Recall must not compare raw scores from different sources.

BM25 scores depend on corpus statistics and implementation. Cosine similarity depends on the embedding model and content shape. SQL match fractions, API relevance values, and hand-authored entity boosts have unrelated scales. Normalizing each result list to zero through one makes them look comparable without making them comparable.

Cross-source ranking should be built in layers.

### 1. Source Eligibility

Decide which sources are allowed and plausibly useful before ranking candidates.

Eligibility can use:

- Explicit user or host scope
- Permission and sensitivity policy
- Exact identifiers or record-type cues
- Time constraints
- Adapter health and latency budget
- Simple query intent rules

Eligibility is stronger than a low source weight. A source that is irrelevant, denied, or too stale should not compete.

The first version should use understandable rules and broad parallel search when uncertain. A learned router can come later.

### 2. Source-Local Retrieval

Each eligible adapter returns its best `k` candidates in local order. Per-source limits prevent a large corpus from flooding the fusion pool.

An adapter may expose match signals, but local rank is the only mandatory relevance signal.

### 3. Rank-Based Fusion

Weighted reciprocal rank fusion is the proposed baseline:

```text
fusion_score(candidate) =
    source_prior(query, source)
    * 1 / (rank_constant + local_rank)
```

If equivalent evidence appears in several sources, the fused cluster receives contributions from each list.

This baseline has useful properties:

- It does not depend on raw score scale.
- One source cannot dominate because its engine emits larger numbers.
- Source priors remain explicit and testable.
- Improvements inside an adapter are reflected through better local rank.

The rank constant and source priors must be evaluation parameters, not folklore.

### 4. Query-Dependent Source Priors

There should not be one permanent weight for a source.

Examples:

- A person-name query should favor the people directory.
- An issue identifier should favor work items.
- "What did we decide?" should favor distilled notes and decision records.
- "What happened after the deploy?" should favor time-aware events and conversations.

Source priors should begin as small, bounded adjustments. They should not rescue poor local results or guarantee that one source always wins.

### 5. Clustering And Corroboration

Candidates that refer to the same entity, event, or fact should be clustered before final display.

Cross-source agreement can increase confidence, but duplication is not automatically corroboration. Two indexes derived from the same original record count as one evidence lineage.

Clustering depends on stable identifiers when available and conservative entity matching otherwise.

### 6. Shared-Scale Reranking

An optional reranker may score the fused candidate pool against the original query on one shared scale. This can be a local model, remote model, cross-encoder, or agentic evidence pass.

Reranking is not required for the first useful version. It adds latency and can hide source-routing mistakes. The system should retain the pre-rerank score breakdown for inspection.

### 7. Diversity And Output Policy

Final selection should prevent near-duplicate results and preserve useful source diversity. Diversity is a selection policy after relevance ranking, not a substitute for relevance.

The output should distinguish:

- High-confidence direct matches
- Related evidence worth inspecting
- Source failures or missing coverage
- No supported recall

Recall must be able to abstain.

## Explainability

Every returned candidate should have a compact score explanation:

```text
source: people
local rank: 1 of 8
matches: exact name, alias
source prior: person lookup
corroboration: one independent event source
freshness: live at 2026-07-23T...
reranker: not used
```

The explanation is part of the product, not only a debugger feature. It makes ranking errors actionable.

## Security And Privacy

- Treat all retrieved content as untrusted data, never as agent instructions.
- Assign trust at Recall's central boundary. An adapter cannot mark
  source-derived text as trusted.
- Strip unsafe control characters, bound text fields, and validate links before
  candidates reach a terminal, API, MCP client, or model.
- Never allow retrieved content to change the retrieval plan, adapter command,
  configuration, permission scope, or available tools.
- Enforce source permissions before retrieval and again before expansion.
- Open databases with read-only credentials or modes by default.
- Do not log full queries or excerpts by default.
- Allow source- and candidate-level sensitivity labels.
- Carry provenance and source revision through every transformation.
- Redact diagnostics so a denied source does not leak record existence.
- Keep adapter secrets outside manifests and result envelopes.
- Make remote embedding or reranking an explicit data-egress decision.

Portable configuration must not mean portable access to every source. A source can be configured on one machine and unavailable or forbidden on another.

## Observability

Recall should record operational facts without collecting query content by default:

- Eligible, searched, skipped, degraded, and failed sources
- Per-adapter and total latency
- Candidate counts before and after fusion
- Expansion success rate
- Index and source watermarks
- Fallback paths used
- Reranker usage and cost
- Suppressed candidate counts and reasons
- Expansion requests and host-reported selection or citation events

Opt-in evaluation logs may retain query text and judgments in a separate, clearly classified dataset.
Aggregate consumption metrics should not require query bodies. Candidate- or
locator-level telemetry is opt-in because identifiers can reveal sensitive
subjects.

## Evaluation

[ADR-0002](adr/0002-evaluation-framework.md) proposes a native evaluation
harness. [The evaluation framework](evaluation.md) defines its packs, schemas,
metrics, gates, reports, and bounded autonomous research loop.

The initial evaluation set should contain real questions spanning all source types. It must include:

- Exact entity and identifier lookups
- Paraphrased topic recall
- Implicit references
- Multi-source questions
- Temporal and changed-fact questions
- Queries with one authoritative source
- Queries where several sources disagree
- Questions with no supported answer
- Unavailable and stale source scenarios
- Permission-restricted results

Useful measures:

- Per-source recall at `k`
- Cross-source MRR or nDCG
- Answer-support rate
- Abstention accuracy
- Unsupported-memory injection rate
- Expansion success rate
- Provenance correctness
- p50 and p95 latency
- Result and reranking cost

The deciding measure is whether Recall improves the agent's final answer without adding misleading context.

The fixture benchmark runs in CI. A separate private benchmark runs before
ranking, decay, indexing, or adapter changes are accepted. Both verify degraded
source behavior and assert that configured policies appear in score
explanations, preventing a setting from existing only on paper.

### Evaluation Before Tuning

Recall should have a versioned evaluation corpus before source priors, decay
constants, score thresholds, or reranker prompts are tuned.

Each case should contain:

```text
case_id                 stable identifier
query                   realistic user wording
scope                   sources and permissions available to the query
as_of                    optional historical or current-state boundary
expected_evidence       acceptable source locators or evidence lineages
forbidden_evidence      known distractors or superseded records
expected_behavior       answer, clarify, or abstain
notes                   why this case matters
```

The expected target is evidence, not only a reference answer. Several different
answers may be valid when they are grounded in the right records.

The first benchmark should compare increasingly capable, independently useful
baselines:

1. Exact and lexical retrieval within each source
2. Source-local retrieval fused with unweighted reciprocal rank fusion
3. Query-dependent source priors
4. Temporal and lifecycle features
5. Semantic document retrieval
6. Optional shared reranking

Each layer must earn its complexity on a held-out subset. A change that improves
average ranking but damages exact identifiers, current-state questions,
abstention, or provenance is not automatically an improvement.

Evaluation should report both aggregate scores and an error taxonomy:

- Source was incorrectly excluded
- Source-local retrieval missed the evidence
- Fusion ranked the right evidence too low
- A stale or superseded record outranked current authority
- Duplicate lineage was mistaken for corroboration
- Expansion returned the wrong revision or insufficient context
- The system answered despite inadequate evidence

Real work questions are necessary, but the evaluation manifest should avoid
copying source bodies. It can retain private queries and stable locators in a
private dataset, then resolve the evidence during a benchmark run. Synthetic
fixtures should cover failure and permission scenarios that are unsafe or
impractical to reproduce against live data.

## Time, Freshness, And Decay

Recall needs to distinguish four different time concepts:

**Event time**
: When a message, meeting, task change, or other event happened.

**Validity time**
: When a fact was or is true. A newer record may supersede an older one without
  making the older record irrelevant to a historical query.

**Observation time**
: When Recall read or indexed the source record. Observation alone does not
prove that a later source boundary completed.

**Confirmation time**
: When a complete successful source operation confirmed that the record was
present or current.

A fifth concept, reinforcement time, exists only inside memory-owning sources
and stays behind their adapters.

These timestamps answer different questions and must not be collapsed into one
`last_seen` field.

### Decay Belongs To Adapters

The Recall core never implements decay. Decay, reinforcement, and archival are
memory-store semantics: they require owning the records, and Recall does not.
An adapter whose source has native decay, such as Clara memory, applies it
inside source-local ranking. Cross-source fusion operates on local ranks, so
decayed weights never leak into cross-source comparison.

This delegation is permanent, not a first-version simplification. If a future
source needs decay, its adapter or its underlying store implements it.

For adapters that add decay to a source without native support, Recall's
documentation is opinionated about how:

- Decay is selected by record semantics, not storage type.
- Durable facts do not become false merely because they are old.
- Only transient observations and inferred preferences lose influence.
- Reinforcement requires new evidence or explicit behavior, not retrieval hits
  and not source refreshes that merely re-observe the same assertion.
- Faded material remains recoverable and searchable for historical questions.
- Half-life and archive thresholds are inspectable policy, tuned from
  evaluation evidence rather than copied from another system.
- A historical query can deliberately retrieve old evidence without a recency
  penalty; contradicting evidence changes validity rather than applying
  negative decay.

The core's temporal obligations are the four timestamp semantics above, the
`as_of` query boundary, and current-state versus historical query handling.

## Initial Source Inventory

The concrete first-deployment inventory is a profile example, not core
specification. It lives in [sources-initial.md](sources-initial.md). The
abstract Source Classes above are the specification.

## Build Versus Adopt

Recall should own the parts that are distinctive to this system:

- Logical source registry and adapter contract
- Eligibility, cross-source fusion, and evidence lineage
- Freshness, permissions, provenance, and expansion
- Temporal semantics and decay policy
- Evaluation corpus, benchmark runner, and regression gates
- Host-neutral query and result contracts

Specialized retrieval engines can sit behind adapters when they solve a hard
subproblem substantially better than a new implementation.

### QMD

[QMD](https://github.com/tobi/qmd) is a strong candidate for the document
adapter's retrieval engine. It already provides local BM25, vector search,
hybrid reciprocal-rank fusion, local model reranking, collections, contextual
metadata, bounded line retrieval, JSON output, an MCP server, and a library
interface.

**Proposal:** Trial QMD behind the document adapter rather than adopt it as
Recall's top-level architecture. Recall would still own source eligibility,
cross-source fusion, provenance, evaluation, and non-document adapters. The
integration must remain replaceable and can use the CLI or MCP boundary so it
does not decide Recall's implementation language.

This is the kind of subsystem worth considering instead of rebuilding
immediately: document chunking, hybrid retrieval, local embedding model
operation, and reranking form a genuinely complex package. The benchmark should
compare QMD with a simple lexical baseline before it becomes a dependency.

### MemoryWiki

[MemoryWiki](https://github.com/MemoryWiki/MemoryWiki) has valuable design
patterns: Markdown-native memory, episodic/semantic/procedural separation,
source hashes, update history, conflict preservation, rebuildable indexes,
write gates, and explicit forget workflows.

**Proposal:** Use it as a design reference, not a dependency for the first
version. It overlaps Recall's memory product boundary, is early-stage, and is
Python-first, while Recall is still deliberately language agnostic. Its
provenance, conflict, and write-safety ideas should inform later memory
construction work.

## Implementation Direction

[ADR-0001](adr/0001-core-implementation-language.md) proposes Go for the Recall
core.

Go is the best fit for the current boundary: one dependable executable that can
act as a CLI, local service, and MCP server while supervising concurrent source
adapters. The official MCP Go SDK is Tier 1, and specialized retrieval systems
such as QMD can remain external processes.

TypeScript on Bun is the strongest alternative. It would make direct QMD
integration and JavaScript adapter development easier, and Bun can package
standalone executables. Its runtime and native-dependency surface is less
predictable than Go's for a long-lived local utility.

Rust is a good choice for a future retrieval engine with measured
performance-sensitive indexing work. It asks for more implementation effort
than this orchestration-heavy first version needs.

The contracts in this specification remain language neutral even if the first
core implementation uses Go. The Go decision should be accepted only after the
ADR's implementation spike passes.

[ADR-0003](adr/0003-adapter-wire-protocol.md) proposes the external adapter
wire protocol: newline-delimited JSON-RPC 2.0 over stdio with a manifest
handshake, cancellation, and recorded-transcript conformance testing.

## Working Decisions

These choices are accepted for the current draft:

- The product name is Recall.
- Recall is reusable across projects and contains no Clara-specific core logic.
- The specification and adapter contracts remain language neutral.
- Recall uses source adapters behind a stable contract.
- Recall supports built-in and out-of-process adapters.
- Source-local retrieval and cross-source fusion are separate concerns.
- Raw relevance scores are not compared across sources.
- Results use progressive disclosure with stable locators.
- Source health and freshness are part of query correctness.
- The first version is read-only.
- The CLI is canonical; any local API or MCP surface is a thin transport over
  the same application core.
- Retrieval evaluation will exist before source weights and decay constants are
  tuned.
- The evaluation runner is native to Recall and exports standard TREC-compatible
  judgments and ranked runs.
- Autonomous research can optimize a development score only after hard
  correctness, privacy, provenance, abstention, latency, and cost gates pass.
- Configuration is TOML; machine-generated output is JSON.
- Evidence lineage is declared: record-level `derived_from`, then source-level
  `derives_from`, with content fingerprints as an advisory collapse signal
  only.
- Locators are stable, printable, and interpreted only by the owning adapter.
- Decay is permanently an adapter concern; the core carries temporal fields and
  the `as_of` boundary but never applies decay.
- Broad parallel search is the default. Eligibility uses hard rules only:
  permissions, health, explicit scope, and exact-identifier routing. No soft
  intent router before evaluation shows routing errors.
- The first vertical slice is two sources: Tasks through its CLI contract
  (live, structured) and Clara project documents (indexed). Fusing two
  heterogeneous sources proves the architecture; more sources follow the
  baseline.
- The external adapter protocol is newline-delimited JSON-RPC 2.0 over stdio
  per [ADR-0003](adr/0003-adapter-wire-protocol.md).
- Evaluation is staged: schemas, smoke pack, run and compare tooling, and hard
  gates come first; research score, holdout governance, and the autonomous
  loop wait until a trusted baseline exists.

## Open Decisions

1. Should QMD be the first document backend trial?
2. What source priors are justified for the first query classes?
3. When is shared-scale reranking worth its latency and data-egress cost?
4. What query and result data may be retained for evaluation?
5. Which host integrations are required first beyond the CLI: library, local
   service, MCP, or another protocol?
6. Does the Go implementation spike satisfy ADR-0001 well enough to accept the
   language decision?
7. Do the initial benchmark distributions support ADR-0002's proposed research
   score weights and acceptance gates?
8. Where does conversation-scoped suppression state live when Recall runs
   per-query in-process rather than as a service?

## Source Discussion: Opening Position

The weighting problem should be reframed as source eligibility, source-local ranking, and cross-source fusion.

Scent tried to produce one comparable confidence number from entity matches, BM25, recency, source type, and semantic similarity. That number carried too many meanings. Recall should let each source answer a narrower question: "Which of my records best match this request?" Fusion then asks: "Given these ranked lists and this query, which evidence deserves the agent's attention?"

My proposed starting point:

1. Define sources by logical retrieval semantics, not file type.
2. Require local rank and provenance from every adapter.
3. Search eligible local sources in parallel with small per-source limits.
4. Fuse with weighted reciprocal rank fusion.
5. Add only modest query-dependent source priors.
6. Cluster equivalent evidence and reward independent corroboration.
7. Add a shared reranker only after the baseline has measured failures.

The first vertical slice is Tasks plus Clara project documents. It covers
indexed documents, structured live records, cross-source fusion, and stable
locators with the smallest surface that can produce a trustworthy baseline.
The first append-only stream source, which adds record-level lineage and
temporal behavior, follows once that baseline exists.

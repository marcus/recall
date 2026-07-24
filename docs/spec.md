# Recall Specification

**Status:** Working draft

**Last updated:** 2026-07-23

Recall is a portable retrieval subsystem for personal AI agents. It searches heterogeneous, user-controlled data and returns compact, source-grounded evidence that an agent can inspect or expand.

The first design problem is source integration. Recall must search text documents, SQLite databases, JSONL streams, and future adapters without pretending that their native relevance scores mean the same thing.

## Product Context

Recall is intended for a new personal agent system that should be more portable than OpenClaw and suitable for work environments. It may run inside different agent hosts over time, so its core contracts must not depend on one runtime, programming language, model provider, or storage engine.

The expected deployment is local-first and single-user at the beginning. Some sources may later be remote or shared. Work data makes provenance, permissions, and query privacy part of the base design.

## Goals

- Search multiple logical sources through one stable interface.
- Preserve source-native search behavior instead of forcing every source into one index.
- Combine source-local result lists without comparing meaningless raw scores.
- Return compact evidence first, with progressive expansion when the agent needs more.
- Keep every result traceable to a stable source locator.
- Make missing, stale, denied, and unhealthy sources visible.
- Support both live retrieval and indexed projections.
- Allow new adapters without changes to the ranking core.
- Measure retrieval quality using real questions and downstream answer utility.

## Non-Goals For The First Version

- Choosing what deserves to become durable memory.
- Editing or synchronizing source data.
- Building a universal knowledge graph.
- Replacing the agent runtime or its conversation manager.
- Training a global ranking model before a useful labeled dataset exists.
- Converting every source into a single canonical storage schema.

Recall may later participate in memory capture or consolidation, but retrieval should stand on its own first.

## Terms

**Source**
: A logical retrieval authority with coherent query semantics, freshness rules, permissions, and provenance. A source is not necessarily a physical file or database.

**Adapter**
: The implementation that connects Recall to a source. It owns source-specific discovery, querying, health checks, and expansion.

**Record**
: A native item exposed by a source, such as a document section, person, task, message, or event.

**Candidate**
: A compact search result returned by an adapter for cross-source fusion.

**Evidence**
: Expanded source material retrieved from a candidate's locator.

**Locator**
: A stable, opaque reference that lets the adapter retrieve a record or evidence range again.

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
source_watermark      latest source revision, timestamp, or cursor
index_watermark       indexed revision when applicable
record_count          exact or estimated
diagnostics           safe operational details
```

An unavailable source must not look like a successful search with zero matches.

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
```

The adapter owns local retrieval and ordering. It may use SQL, FTS, vector search, exact lookup, an API, or a combination.

### Candidate Envelope

Every candidate includes:

```text
candidate_id          stable within the source revision
source_id             logical source identifier
locator               opaque expansion reference
record_type           person, task, document, message, event, ...
title                 compact human-readable label
excerpt               bounded evidence preview
local_rank            mandatory rank in the source result list
local_score           optional source-native score
match_signals         exact, lexical, semantic, field match, filters
observed_at           when Recall observed this record
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

Opt-in evaluation logs may retain query text and judgments in a separate, clearly classified dataset.

## Evaluation

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

## Working Decisions

These choices are accepted for the current draft:

- The product name is Recall.
- The specification remains language agnostic.
- Recall uses source adapters behind a stable contract.
- Source-local retrieval and cross-source fusion are separate concerns.
- Raw relevance scores are not compared across sources.
- Results use progressive disclosure with stable locators.
- Source health and freshness are part of query correctness.
- The first version is read-only.

## Open Decisions

1. What logical sources will the first deployment include?
2. Which sources must be live, indexed, or hybrid?
3. How are adapters registered and configured portably?
4. What source priors are justified for the first query classes?
5. Should broad parallel search be the default, or should a router narrow most queries?
6. What constitutes the same evidence lineage for corroboration?
7. Which stable identifiers exist across sources?
8. When is shared-scale reranking worth its latency and data-egress cost?
9. What query and result data may be retained for evaluation?
10. Which host integrations are required first: library, CLI, local service, MCP, or another protocol?

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

The next discussion should inventory the first real sources. For each one, we need to name its logical records, authority, freshness needs, best native retrieval method, stable identifiers, sensitivity, and likely query classes. That inventory will tell us whether the proposed adapter contract is correct before implementation begins.

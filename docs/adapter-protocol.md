# Recall Adapter Protocol

**Version:** 1 — normative

This document defines the adapter contract: message shapes, transport, and
conformance. See the [specification](spec.md) for core behavior.

Built-in and external adapters implement the same contract. Built-in adapters
implement the Go interface directly; external adapters implement it over
JSON-RPC. There is one contract with two transports, and both are exercised by
the same conformance suite.

## Transport

Newline-delimited JSON-RPC 2.0 over stdio.

- One JSON-RPC message per line on stdin/stdout. Messages contain no raw
  newlines.
- stdout carries protocol frames only. stderr is free-form adapter logging and
  is captured into diagnostics, never parsed.
- The core is the client; the adapter is the server.
- Payloads are validated against JSON Schema on both sides.

Recall messages are compact by design — candidates are pointers and expansion
is budget-bounded — so line framing costs nothing and keeps transcripts
diffable. A future network transport reuses these messages unchanged over a
socket or HTTP.

## Methods

```text
recall/initialize   handshake: version range, workdir, source_id, location,
                    settings -> manifest, capabilities
recall/search       search request -> candidates envelope
recall/expand       locator + budget -> evidence
recall/health       probe -> health report
recall/refresh      bring an adapter-owned projection up to date -> health
recall/cancel       notification: abandon a request id
recall/shutdown     request clean exit
```

Rules:

- Version negotiation happens once, in `recall/initialize`. An adapter that
  cannot satisfy the requested range fails the handshake explicitly rather than
  degrading.
- Every request carries a deadline. `recall/cancel` is advisory; the core
  enforces deadlines with SIGTERM then SIGKILL and reports the source outcome
  as `timeout` or `failed`, never as empty success.
- An adapter process is long-lived and pooled per source instance. It must
  tolerate concurrent in-flight requests or declare `max_concurrency: 1` in its
  manifest.
- `recall/initialize` supplies a writable `workdir` under Recall's state
  directory. An adapter writes indexes and checkpoints only there.
- `recall/initialize` supplies the already-resolved `location`. A source may
  declare `location_kind = "path" | "opaque" | "uri"`; that discriminator is
  authoritative, so slash-bearing mailbox/device identifiers reach the adapter
  unchanged and foreign Windows paths remain expressible without pretending
  either is a URI. Legacy entries without a kind use the documented URI-first
  compatibility inference. Adapters must treat opaque locations as identifiers,
  not paths. `base_dir` remains the anchor for paths inside adapter-owned
  settings.
- `recall/initialize` also supplies the instance's configured `source_id`,
  because locator text is `<source_id>:<local>` and adapters write their own
  locators. This is a name, not identity: the core attaches `source_uid` and
  overwrites the source part of every locator an adapter returns, so a forged
  prefix cannot make one source answer as another. Only a `derived_from`
  prefix is read at face value, and an unknown one is dropped.
- `recall/cancel` takes `{"id": <request id>}`. `recall/shutdown` takes `{}`
  and returns `{}`.
- `recall/refresh` is what the `checkpoint` capability means. Only adapters
  declaring it serve the method; an adapter owning no projection returns its
  health unchanged rather than reporting work it did not do. A build that fails
  is reported through the returned health — stale watermark, degraded status,
  reason in diagnostics — and never as an error: a frame carries a result or an
  error and never both, so erroring would discard the health of the generation
  that is still published and still answering. An error means the refresh could
  not be performed at all. Without this method the only in-contract place to
  build an index was the handshake, which competes with the handshake timeout
  on any real corpus.

## Errors

JSON-RPC error codes, plus Recall codes in the implementation-defined range:

```text
-32000  source_unavailable        cannot reach the source
-32001  source_denied             permission refused
-32002  locator_unknown           locator does not parse for this adapter
-32003  locator_expired           source changed incompatibly
-32004  source_not_configured     locator names an unconfigured source
-32005  as_of_unsupported         historical boundary cannot be honored
-32006  budget_exceeded           adapter declines up front: cannot serve in
                                  the budget offered
-32007  deadline_exceeded         the request ran out of time, or was abandoned
                                  in flight
```

Errors carry safe diagnostics only. A denied source must not leak record
existence.

## Manifest

Returned from `recall/initialize`:

```text
protocol_version     negotiated version
adapter_id           implementation identity and version
display_name         human-readable name
record_types         person | task | document | message | event | ...
query_modes          exact | lexical | semantic | structured | temporal
freshness_modes      live | indexed | hybrid  (supported set)
as_of_support        none | filter | snapshot
derives_from         optional source_id this source projects wholesale. A
                     name, not an identity: an adapter cannot know another
                     instance's generated source_uid. The core resolves it and
                     drops the edge when the named source is not configured.
capabilities         search | expand | enumerate | checkpoint | context_expansion
max_concurrency      optional, default unbounded
freshness_policy     expected refresh or verification behavior
sensitivity          default classification floor
settings_schema      JSON Schema for the instance's settings block
```

`settings_schema` arrives in the initialize *result*, after the core has
already sent `settings`. On a first handshake the adapter therefore validates
its own settings and fails the handshake with a readable error. The core
validates against the schema it already holds on every later handshake, and
`recall doctor` uses it to check configuration without starting a query.

The manifest describes the adapter's capabilities. Identity, priors, and
policy come from configuration; an adapter never names its own `source_uid`.

## Health

```text
status               healthy | degraded | unavailable | denied
checked_at           probe time
last_success_at      latest complete successful operation
source_watermark     latest source revision, timestamp, or cursor
index_watermark      indexed revision, when applicable
index_generation     published generation identity, when applicable
index_model          embedding model id and version, when applicable
index_config         identity of the retrieval configuration this generation
                     was built under: analyzer, tokenizer, scoring parameters.
                     index_model covers embeddings only, so without this a
                     change to scoring silently changes ranking with nothing in
                     the generation recording it, and an evaluation comparing
                     two generations would credit the change under test.
record_count         exact or estimated
indexed_count        records represented by the current index
failed_count         records rejected or not indexed
coverage             complete | partial | unknown
diagnostics          safe operational detail
```

An unavailable source never looks like a successful search with zero matches.
A partial source or index reports `healthy` only when its declared freshness
policy permits that exact partial boundary. A recent index timestamp alone is
not health.

### `store_identity`

One diagnostic key has a defined meaning across adapters:

```text
diagnostics.store_identity   the store THIS INSTANCE opened, named by the
                             adapter; optional
```

`recall doctor` fails a profile in which two enabled instances of one adapter
report the same value. The check exists because "two sources, one store" is a
configuration error no single source can see: lineage groups on `source_uid`
plus `source_record_id`, so one record reaching the core through two instances
arrives as two independent pieces of evidence and collects the corroboration
bonus for agreeing with itself.

Three rules make the key mean something:

- **It names what was opened, never what was configured.** A value copied from
  the configured location compares equal exactly when the configuration is
  already consistent, which is the case that was never in doubt. A td source
  configured at a repository and another at a subdirectory of it are one
  database, and only the resolved database says so.
- **Setting it claims exclusivity.** An adapter for which two instances over
  one store is a legitimate configuration — one serving different views of a
  shared catalog, whose candidates collapse on a `content_fingerprint` — leaves
  it unset. Absent means "makes no such claim", never "unknown".
- **It is compared only within one adapter, and only for equality.** Its
  content is otherwise opaque: a resolved path, a database identity, an
  endpoint plus a namespace are all fine.

An adapter that claims a store should also carry a `content_fingerprint` on its
candidates, so that a duplicate configuration is corrected by the operator
*and* harmless until they do.

## Search

Request:

```text
query                original user query text
context              optional bounded prior message texts; honored only when
                     the manifest declares context_expansion
filters              time, record type, entity, project, source scope
as_of                optional historical boundary
limit                maximum candidates requested
deadline             absolute deadline
```

The core sends the query text as given. It does not synthesize query variants;
term expansion, stemming, and synonyms are source-local concerns owned by the
adapter.

Response:

```text
candidates           ranked source-local candidates
diagnostics          timing, query mode used, fallback, truncation
source_watermark     freshness evidence for this search
outcome              success | partial | unavailable | denied | failed |
                     timeout | skipped
reason               why, when the outcome is skipped; closed vocabulary
```

### Saying "this does not apply to me"

`skipped` is how an adapter reports that the request named something it does
not serve. It exists because the alternative was worse: without it the only way
to say so was `success` with no candidates, and **success asserts a boundary
that was crossed and found empty**. A `project` filter naming a workspace no
source has therefore came back as `coverage: complete` over a search that never
ran — a false absence wearing the one outcome guaranteed not to degrade.

An adapter that skips states a `reason`, and the core decides what it costs:

```text
not_applicable        the request named a project, entity, or scope this
                      source does not serve. Only the source knows what it
                      answers to, so the core cannot derive this.
filter_unsupported    the adapter could not EVALUATE a filter it was given,
                      as opposed to evaluating it and matching nothing.
record_type_mismatch  the request asked for types this source does not hold.
as_of_unsupported     the historical boundary cannot be honored.
```

`not_applicable` and `record_type_mismatch` do not degrade a response on their
own: one source not being the one asked for is routing working as intended.
`filter_unsupported` does, because what the adapter would have returned is a
superset or a subset of the request and it cannot say which.

An adapter must decide this before retrieval and return zero candidates.
Returning broader candidates as `partial` does not make them evidence for the
filtered question. The core also discards any candidates attached to a
`skipped` response, so a malformed external adapter cannot leak out-of-scope
evidence into fusion.

Two rules keep this from becoming a cheaper way to look healthy:

- **A reason outside the vocabulary is treated as unstated, and unstated
  degrades.** An adapter cannot invent a spelling that lands in the
  non-degrading branch.
- **When NO source was applicable, the response degrades anyway.** Per source,
  non-applicability is routing; over a whole response it means the request
  named a boundary this machine does not have, and `complete` would claim the
  system searched everywhere for it.

`skipped` is never a way to report a failure. A source that could not be
reached, ran out of time, or refused says so with its own outcome.

The adapter owns local retrieval and ordering, by SQL, FTS, vector search,
exact lookup, an API, or any combination.

## Candidate Envelope

```text
candidate_id         stable within the source revision
source_record_id     stable identity of the RECORD, not of this candidate.
                     Chunks of one document, or several hits inside one task,
                     all carry the same value. Corroboration collapses on it,
                     so a per-chunk value would let one source corroborate
                     itself.
locator              stable printable expansion reference
derived_from         optional upstream locators, for lineage
record_type          person | task | document | message | event | ...
title                compact human-readable label
excerpt              bounded evidence preview
local_rank           mandatory rank within this source's result list
local_score          optional native score, diagnostic only
match_signals        exact_identifier | lexical | semantic | field | alias
observed_at          when Recall observed this record
confirmed_at         when a complete source boundary last confirmed it
event_time           when the source event happened, if applicable
valid_from           optional fact validity start
valid_to             optional fact validity end
source_revision      revision or watermark used for retrieval
sensitivity          candidate classification; may raise the source floor,
                     never lower it
metadata             small typed fields for routing and display
content_fingerprint  optional normalized content hash, advisory only
```

`source_uid` is attached by the core, not the adapter. `local_score` is never
compared across sources.

An adapter emits `exact_identifier` only for an exact match on a stable
identifier or a declared alias, at token boundaries. Unbounded substring
matches never carry this signal.

## Expand

Request:

```text
locator              candidate reference
detail               summary | excerpt | full | context
budget_bytes         hard output limit in bytes
deadline             absolute deadline
```

Response:

```text
content              evidence text
source_revision      revision the evidence was read from
truncated            bool
truncation_boundary  which limit applied, so a caller can tell a budget cut
                     from a source-side limit
provenance           path, range, or record reference
```

Expansion fails with `locator_expired` when the source changed incompatibly.
It never silently returns a different revision or a nearby record.

## Index Obligations

An adapter maintaining an index must satisfy the index rules in
[spec.md](spec.md#index-obligations): atomic generation publication after a
complete source boundary, durable-then-checkpoint ordering, model identity per
generation, and deletion honored on publication. Health fields are how the core
verifies these; the evaluation gates assert on them.

## Conformance

Conformance is recorded transcripts. Each adapter ships:

```text
conformance/
  <case>/manifest.json      how to replay this case
  <case>/request.jsonl      one JSON-RPC message per line
  <case>/response.jsonl     expected responses, order-significant
  <case>/fixture/           source data for the case
```

A transcript that only fixed those file names would not be replayable, because
three things about a recorded exchange are specific to the machine that
recorded it. The manifest settles them:

```json
{
  "case": "handshake",
  "description": "what this case demonstrates",
  "flow": "lockstep",
  "placeholders": {
    "FIXTURE": "absolute path of this case's fixture/ directory",
    "WORKDIR": "absolute path of a fresh, writable, empty directory"
  },
  "volatile": ["/result/checked_at", "/result/last_success_at"],
  "responses": 2
}
```

- **Placeholders.** Requests carry `${FIXTURE}` and `${WORKDIR}`, substituted
  textually before parsing. Without them a transcript is bound to the absolute
  paths of the machine that recorded it.
- **Deadlines are fixed, not substituted.** Every recorded request states a far
  future deadline, so replay is time-independent and the harness never rewrites
  a request field.
- **`flow: lockstep`.** Send lines in order; after a *request*, wait for its
  response before sending the next *request*; send *notifications* immediately.
  The notification exemption is what makes a cancellation case recordable at
  all — waiting for the response to the search being cancelled would deadlock.
  Then close stdin and drain; anything read while draining counts.
- **`volatile`** lists RFC 6901 JSON Pointers from the root of the frame, where
  `*` matches any array element or object member. Both sides are masked before
  comparison. Declare a field volatile only when the adapter cannot control it:
  a timestamp the fixture states is not volatile.
- **`responses`** is the expected count, so a case that stops answering fails
  rather than passing short.

`recall doctor --conformance <adapter>` replays each case and diffs responses,
ignoring declared-volatile fields (timestamps, latency). It finds the suite
through the adapter's registration, which names the directory in
`conformance`; see [adapter registration](spec.md#adapter-registration). An
adapter that declares none is reported as unchecked rather than as passing,
because "verified nothing" must never read as "verified". Required cases:

- Handshake, including a version-range rejection.
- Search returning ranked candidates with stable locators.
- Search returning `partial` with honest coverage.
- Search against an unavailable source.
- Expand at each detail level, including a budget truncation.
- Expand with an expired locator.
- Cancellation of an in-flight request.
- Clean shutdown.

The same transcripts serve evaluation determinism: a fixture pack replays
recorded adapter responses instead of running live sources, so model-backed and
network-backed adapters produce reproducible benchmark runs.

## Writing An Adapter

An external adapter is an executable that reads JSON-RPC lines on stdin and
writes them on stdout, implementing six handlers. Any language with a JSON
library suffices; debugging is `echo` and `jq`.

The core owns spawn, handshake, deadline enforcement, kill, restart policy, and
pooling. Adapters own retrieval, ranking within their source, indexing, and
locator semantics.

Adapter commands are declared only in user-level configuration. See the
[trust boundary](spec.md#layers-and-trust-boundary).

[writing-an-adapter.md](writing-an-adapter.md) is the working guide — what this
contract's rules prevent, and a copyable template that satisfies them.

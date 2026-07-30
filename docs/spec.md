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
   onto a shared scale. Cross-source fusion uses rank, and `relevance` — which
   is not a raw score and not normalized onto a shared scale: it is reported in
   [0,1] against one fixed DEFINITION every source computes the same way. A
   source's own number stays diagnostic. The distinction is the whole of why
   this invariant survives the addition: nothing here rescales anybody's
   scoring, and a source that declines to report relevance is not disadvantaged
   by the arithmetic.
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
budget               latency_ms, response_tokens, and the surface
                     the token budget is denominated in
limit                maximum fused results. Unset AND 0 both take the
                     profile's max_results; a negative limit is refused
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
source_summary       stands in for source_outcomes under a response budget
plan                 the resolved retrieval plan
suppressed           counts and reasons for withheld candidates
omitted              what the response budget removed from the frame
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

Built-in adapters may overlap a request's health work with retrieval when
their source-specific contract can do so safely. That does not reverse the
decision boundary: the health result and store identity still decide whether
the source may answer, speculative candidates are discarded on any
disagreement, and the request deadline cancels both sides. The optimization is
observable only as latency; it cannot change coverage or make an unverified
store's evidence admissible.

`truncated` means budget shaping dropped trailing results. Truncation is not
degradation, and it is not an abstention either: what the corpus said is
decided before shaping. See Response Budget.

Each result carries its primary candidate, cluster members with lineage roots,
a structured score explanation, and a locator. The primary is the record's
display representative; the explanation names the evidence that produced its
score. Those are normally the same candidate. They can differ when one
document's preview-only chunk earned the record's score but another chunk of
that same document carries a matched excerpt worth showing. Leading results
include excerpts; trailing results compress to one line plus locator. The same
structure serializes to JSON and renders as tiered text. Neither surface gets
extra fields, and neither may assert what the structure does not say.

An excerpt says which of two things it is. `excerpt_kind: matched` is the span
the query matched; `excerpt_kind: preview` is the record's opening, shown
because nothing in its text matched — the query named the document outright, or
matched a field the excerpt does not carry. Absent is a third state and not a
default: the source asserts neither, either because it does not select excerpts
by query or because it could not read the record to find out. All three are
distinguishable without `--explain`, because a caller shown the head of a record
has no way to tell a real hit from a false one and re-deriving it by hand is the
cost.

A matched excerpt is a claim about a revision, so it must be selected from text
that revision actually holds: the same query against the same generation always
yields the same window, or it yields none. An adapter that cuts the window from
live content verifies the content is unchanged first — byte for byte, since a
normalized comparison passes on a reflow that moves every offset.

### Output Tiers And Parity

The parity rule is that the human and machine surfaces carry the same facts. It
is read as **the same facts are available from the surface**, not that any one
view prints all of them.

The tier and the encoding are independent. `--explain` picks the tier: pointers,
or pointers plus diagnostics. `--json` picks the encoding: text for a reader, or
JSON for a program. Both renderings are projections chosen for their reader at
the pointer tier, and both are complete at the diagnostic tier.

```text
tier \ encoding    human                    --json

pointer            outcome, coverage, and   the same facts as JSON, plus
(default)          one pointer per result   source_id and record_type per
                   — rank, locator, title,  result so a consumer routes
                   excerpt, plus `exact`    without parsing a locator;
                   and the corroboration    tier: "pointer"
                   count

--explain          the above plus           the whole response, every field,
                   per-result provenance,   nothing projected
                   cluster lineage, score
                   explanations, per-source
                   outcomes, and the plan
```

`--json` alone was the complete serialization until the pointer projection
existed, and the sentence that said so — "the whole response, unprojected and
identical under every rendering flag" — is what this section replaced. The
complete serialization is now `--json --explain`, byte for byte what `--json`
used to emit, so a consumer that needs a projected-out field has an exact
migration rather than a reconstruction.

The change was made because the argument for projecting the human surface, set
out below, is stronger on the machine one and had never been applied to it. On
`recall query dentist`, four results: 22,698 bytes, of which the four primaries
were 3,226. The per-source ledger and plan were 8,478 — unchanged by `--limit`,
identical on a query that found nothing — and `members[].candidates[]`
re-serialized each primary verbatim for 8,247 more, across four clusters that
were all singletons. Held against result count the total was flat: a
twenty-result query serialized to within a few hundred bytes of a four-result
one, which is the diagnostic that the payload was not the answer.

The two markers are the documented exceptions to "pointer only", and each is
there because it states something the position in the list does not. `exact` is
what makes a `preview` excerpt legible: a record whose text did not match, shown
because the query named it outright, is a strong result rather than a suspicious
one, and without the marker the two are indistinguishable. The corroboration
count separates one cluster standing on several independent records from a
single record, which is the distinction the whole ranking layer exists to draw.
The fusion score is deliberately not among them: it is ordinal and uncalibrated,
so the rank is the whole of what it says to a caller choosing what to expand,
and `--explain` prints the number with the arithmetic behind it.

The reasoning, recorded because the question recurs:

- **The default view exists to be chosen from.** Its reader is deciding which
  locator to expand, and `recall expand` takes exactly that locator. Printing
  everything at equal weight made the caller pay for the whole response before
  reading the first result: the source outcomes and plan alone were a fixed
  ~6.5 KB on every query, unreduced by `--limit`, and were the entire cost of a
  query that found nothing.
- **Available, not omitted.** Every fact a pointer tier drops is one flag away
  and unconditionally present under `--explain`, on either encoding. A
  projection that made a fact unreachable would break the rule; one that ranks
  facts by whether they change a reader's next action does not. The flag is the
  same one on both encodings on purpose: a caller who has learned that
  `--explain` means "and tell me why" does not learn a second vocabulary for
  having asked in JSON.
- **What a response CLAIMS is exempt, because its absence is itself a claim.**
  The outcome, the coverage, any source that could not answer, any suppression,
  and anything a budget omitted print unflagged in every mode and every
  encoding. A source that could not answer, or a record withheld, reads as an
  answer that had nothing more when it is not stated — which is the one thing
  this system does not do. This is why the JSON pointer tier drops the
  per-source ledger but never the summary standing in for it: the ledger is
  freshness evidence that `recall sources` answers on demand, while the
  degraded list inside the summary is the response's own account of how
  complete it is.
- **One flag, not two.** `--explain` already means "why did this answer come out
  this way", and every block behind it answers that question. A second
  verbosity flag would split the diagnostics across two axes and leave a caller
  learning which flag holds which fact. `recall sources` and `recall doctor`
  remain the direct answers when source health is the question rather than a
  footnote to a query.

What this forbids is unchanged: a rendering flag may not alter outcome,
coverage, or exit code; no fact may be reachable only from a human tier; and no
tier may state something the structure does not carry.

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
request. Timed-out sources report `timeout`, never empty success, and the
source report names whether the effective deadline came from the request
latency budget or `source.timeout_ms`. If an adapter returns a timeout before
that effective deadline fires, the report says `adapter_internal` instead of
blaming either core budget.

### Response Budget

`response_tokens` bounds the whole rendered response, not its excerpts. A
budget charged against fields rather than against output budgets a response
nobody prints: the same result costs about a hundred tokens as a pointer and
closer to two thousand serialized, and the frame around it — the outcome line,
the per-source ledger, the plan — is not free either.

So the request names the surface the budget is denominated in, and the surface
prices its own rendering:

```text
budget.surface = structured | tool | pointer | explained
```

`structured` is the response serialized whole — `--json`, the HTTP body — and
it is the default: it is what a caller receives when nothing projects it, and
it is the most expensive rendering, so a surface the core does not recognize is
priced as this one and can never let a projection of it overrun. `tool` is an
MCP tool result, which delivers the structured response AND its text projection
inside a JSON-RPC envelope, and is priced as all three. `pointer` and
`explained` are the CLI's two human tiers.

The surface is the caller's declaration of what it will consume; a transport
supplies the default and validates the vocabulary. Over HTTP, a request that
declares nothing is priced as the body the server sends, which is what keeps an
undeclared caller from being handed an unbounded one. A client that renders a
projection of that body declares the projection and is priced for it: `recall
query --server` receives JSON and prints pointers, and pricing it as the body
would make the same query answer differently in process and over a socket — the
substitution `recall serve` exists to make total would stop being total. A
caller that declares a projection it does not apply misprices only itself.

An MCP tool result is the exception, and not by policy: it is consumed where it
is produced. The model reads exactly the bytes the server serialized, so there
is no projection for a declaration to name and the wire form is the only honest
price. `tool` is therefore refused from an HTTP caller, along with any value
outside the vocabulary — this server is not producing a tool result, and
pricing one on that caller's behalf is not something it can do.

Shaping is deterministic and spends in this order:

1. The frame — everything the surface prints whatever it finds — is charged
   first. Footers are inside the budget, not exempt from it.
2. When the frame does not fit, or when keeping it whole would leave no room
   for even one result, its diagnostics summarize: `source_outcomes` is
   replaced by `source_summary`, and the plan's per-source list is dropped.
   Both are named in `omitted`. A response that spent its whole budget on
   diagnostics and answered nothing is the failure this budget exists to
   prevent.
3. Leading results keep their excerpts, greedily from the top.
4. The remainder compress to a title and locator.
5. The tail is dropped, `truncated` is set, and `dropped_results` counts it.

What is never traded away is the minimal floor: the outcome, the coverage,
every degraded source by name, every suppression, and the summary standing in
for what was dropped. Those are claims about the evidence, and a response that
dropped one to save tokens would be cheaper by being less true. On the
eighteen-source home profile the floor measures 17 tokens as pointers, 79 with
`--explain`, and 129 serialized — tens of tokens, not the 1,600 and 2,900 the
full frames cost.

`source_summary` keeps what the ledger is read for: how many sources reported
each outcome, and which of them could not answer, by name and reason. What it
drops is the per-source freshness evidence, which `recall sources` answers on
demand. An omitted fact is always named in `omitted`; an unnamed absence would
read as a source that was never asked.

The outcome is decided before shaping, on what the corpus returned — a budget
too small for one result reports `answered` with `truncated`, never
`abstained`, because the caller's budget is not evidence about the corpus.

The tolerance is one-sided: measured in the same estimator the shaper spends
(about four characters per token, deterministic by design and not calibrated to
any tokenizer), a rendered response never exceeds its budget except by the
minimal floor above, and falls short of it by at most the cost of the first
result that did not fit.

One acceptance criterion this design amends. It was written as
"`--budget-tokens 500` is never larger than `--limit 3`", from an observation
that 500 tokens rendered about 3,400 while `--limit 3` rendered 11.9 KB — the
budget flag naming the smaller number and producing the larger response. With
the response priced as what it renders, the literal comparison is wrong: the
two flags name different units, and 500 tokens legitimately buys more than
three pointer results, which cost about 250. What replaces it is the property
the original was reaching for — a response never exceeds what was asked for:
`tokens(--budget-tokens N) <= max(N, tokens(--limit 3))`, asserted on all three
surfaces.

An unset `response_tokens` is unbounded in the core and
`DefaultResponseTokens` — 8000 — at every product surface: the CLI, the HTTP
API, and the MCP tools all substitute it before the request is served. A
library caller holding the struct pays no rendering cost, so nothing is
withheld from it; a caller with a terminal or a context window is the other
case, and one unset budget once produced a 203 KB response. A negative value is
unbounded outright — `--budget-tokens -1` — which is what makes the ceiling a
default rather than a limit.

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

4. **Duplicate view.** Two candidates agreeing at once on `source_record_id`,
   `content_fingerprint`, and `source_revision` are one record reached twice,
   whatever their `source_uid`s. They cluster for display and collapse for
   corroboration, and the view that does not take the result slot is reported
   in `suppressed` with reason `duplicate_view`, keeping its own lineage root
   and naming the result it was folded into in `fused_into`. The count is
   result slots — one per folded view, whatever number of candidates that view
   arrived as.

   Two views are one record for every question asked of the cluster, not only
   for display. Pre-reply suppression keyed on lineage roots reads them as one:
   a host that has been shown either root has been shown the record, and the
   cluster does not come back under the other. Scoring reads them as one
   position: `fused_into` is what lets an evaluation credit a judgment naming
   either root to the single slot the caller received, rather than counting one
   result twice and measuring everything behind it at a rank nobody saw. A
   response budget that drops the result drops the suppression with it, because
   a withheld view of a record the caller never received would read as more
   evidence held back than there was; one that compresses the result to a
   pointer keeps it, and the folded view stops being expandable from the
   response along with every other member.

   This is the only cross-source agreement that reaches display, and it is
   narrow deliberately. A source instance may be a view over another —
   `projects` and `projects-attention` are one adapter over one catalog scan,
   differing by a filter — and nothing in the two records distinguishes them, so
   a caller asking one question spent two result slots on one project. Two
   alternatives were considered: letting an instance declare in configuration
   that it is a view of another, and simply not making both eligible in one
   profile. The first adds vocabulary for a fact the records already state; the
   second leaves the next overlapping pair to be found by hand. Three
   simultaneous agreements are evidence, not a collision: an echoing source can
   copy text, but copying the upstream record's native identifier and its
   revision as well is being the same record. An adapter that knows two records
   are one thing for a reason not listed here declares `entity_id`.

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

### Machine Defaults

`[defaults]` is user-layer only, for the reason above: every key in it applies
to sources a project did not declare.

```text
profile             profile resolved when a request names none
timeout_ms          per-source query budget for sources declaring none
fusion_reserve_ms   held back so fusion runs inside the request deadline
max_results         result budget for a request that named no limit; 0 is
                    unbounded
relevance_floor     in [0, 1). Withholds a result nothing in which reaches
                    it — unless that is every result, in which case it
                    withholds nothing (Ranking §7). 0 shows everything.
```

`max_results` and `relevance_floor` are the two rules of Ranking §7, and they
are configuration rather than constants because they are policy about how much
of an answer a caller wants, not arithmetic about which record is better. A
value out of range is refused, never clamped, so a machine cannot rank
differently from the configuration that was reviewed.

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
location_kind       optional path | opaque | uri discriminator
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

`location_kind` is authoritative when declared:

- `path` resolves relative paths from the file that declared them, expands `~`,
  and preserves foreign Windows drive or UNC syntax on a non-Windows host.
- `opaque` passes the value byte-for-byte, including mailbox or device
  identifiers containing `/` or `\`.
- `uri` passes a syntactically valid URI byte-for-byte. One-letter schemes such
  as `x:opaque` are valid and remain URIs.

Existing configuration without `location_kind` remains accepted. Its
compatibility rule is syntax-only and URI-first: a URI scheme is `uri`; explicit
filesystem syntax (`/`, `./`, `../`, `~`, a path separator, or UNC form) is
`path`; everything else is `opaque`. Recall never guesses from whether a file
happens to exist. Values that are inherently ambiguous must opt in: use
`location_kind = "opaque"` for a slash-bearing identifier and
`location_kind = "path"` for Windows drive syntax such as `C:\Mail`.
Consequently, a bare project directory may either be written `./notes` or
declared as `path`. `recall config explain` reports the original and resolved
values, effective kind, whether the kind was explicit, and whether path
resolution rewrote the value. Invalid kinds and `uri` values without a scheme
fail configuration loading rather than silently changing meaning.

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

The built-in documents adapter applies one versioned query analyzer before
lexical scoring. It folds alphanumeric terms and removes a bounded list of
English articles, copular/do auxiliaries, and interrogatives: the grammatical
shell that turns keywords into a question. Those terms may not be the only
evidence that opens a result set: every returned non-exact candidate must
contain at least one retained content term itself. Once a candidate meets that
eligibility rule, the full query remains its ranking input, preserving
established positive-query ordering among content-bearing candidates.
Grammatical scaffolding can influence their order but cannot manufacture an
unrelated candidate elsewhere in the corpus. This is query normalization, not index censorship:
exact path and alias matching still sees every raw token. Double-quoted terms
are kept, all-function-word queries fall back to their raw terms, and pronouns,
prepositions, conjunctions, modals, intent verbs, negation, demonstratives,
temporal and directional terms are never discarded. The list contains English
words only; tokens in other languages and scripts pass through unchanged.
Diagnostics report the analyzer and how many terms it removed, and
`index_config` names its version.

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

A refresh may report monotonic checkpoint progress separately from health.
Successful comparable counters that advance with none regressing make the
maintenance operation successful even when a source changed during the pass;
the attached health and coverage remain partial until the index converges.
Unknown, unchanged, regressed, or failed boundaries never qualify as progress.

Adapters receive a writable `workdir` at handshake for index storage. Recall
never resurfaces a record its source no longer contains, except through a
source whose contract retains history, such as an archive stream.

## Ranking

### 1. Eligibility

Eligibility uses hard constraints only:

- Explicit scope from the request. A `source` scope names members of the
  resolved profile. Naming only sources the profile does not contain — whether
  configured elsewhere or not configured at all — is refused as an
  unsatisfiable scope rather than answered, because the alternative asserts
  `coverage: complete` over a source set the scope narrowed to zero. A scope
  that names some members and some non-members answers from the members and
  reports the rest as `out_of_profile`, which degrades coverage. Every other
  scope key naming nothing is a true absence and stays one: it constrains
  records, not the source set.
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
rank is the only MANDATORY relevance signal. `local_score` is diagnostic and is
never compared across sources, because scales differ between engines.

The per-source limit bounds the pool and decides nothing about the answer. It
is a cap on what one source may contribute, so on a profile with many sources
it is an upper bound on candidates and not a statement about how many results a
question deserves. What decides that is §7.

`relevance` is optional and IS compared across sources, because what is fixed
is its definition rather than its scale: every source reports coverage times
concentration, in [0,1], over the same measurement rules (see
`docs/adapter-protocol.md`). A source that omits it is read as 1.0 and so keeps
the ordering it had before the field existed.

### 3. Lineage Grouping

Group candidates by lineage root. Within a group keep the best `local_rank` per
source and select a score-basis candidate: highest source prior, ties broken by
locator sort for determinism.

```text
lineage_score(g) = max over sources s in g of
    prior(query, s) * relevance(s, g) / (rank_constant + best_local_rank(s, g))
```

Maximum, not sum: the same record seen twice is not more evidence.

`relevance(s, g)` is the relevance of the candidate that earned `best_local_rank`
for that source, not the best relevance found anywhere in the group — scoring a
group with one candidate's rank and another's relevance is an arithmetic no
candidate supports. It is 1.0 when the source reported none.

Relevance is a FACTOR on the prior rather than a term beside it, so a source
trusted twice as much and matched half as well lands in the same place. Without
it every source's rank 1 enters this formula identically, and the prior alone
decides which rank 1 wins — a static belief about a source deciding a question
about a record.

### 4. Clustering And Corroboration

Merge lineage groups referring to the same entity, event, or fact — stable
identifiers first, conservative matching otherwise. Entity matching never uses
unbounded substring tests; adapters supply exact identifiers, token-boundary
matches, aliases, or typed resolvers, and test known false positives.

Corroboration counts **units**, not lineage groups. Distinct lineage roots are
not sufficient evidence of independence: two chunks of one document and two
fingerprint-identical records have distinct roots but are one thing said once.
Groups collapse into one unit when they share `source_uid` plus
`source_record_id`, `record_type` plus `content_fingerprint`, or all of
`source_record_id`, `content_fingerprint`, and `source_revision`. A unit's score
is the maximum of its groups'. Only the last of the three also clusters for
display; the reasoning is under Lineage above.

The strongest unit still decides the cluster score and score explanation.
Normally its strongest group also represents the result. One narrow display
rule applies to document chunks: when that group offers only a `preview` and a
different chunk of the same `source_uid` plus `source_record_id` offers a
`matched` excerpt, the strongest such matched chunk represents the document.
The preview chunk still earns the score and remains in the members, so no
heading term becomes unreachable and no score is attributed to evidence that
did not produce it. Empty `excerpt_kind` is neutral, not a preview. The rule
does not cross source-record boundaries and does not apply to structured
records.

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
above all clusters without one for an identifier-shaped or terse one-token
query, ordered among themselves by `cluster_score`. This is a deterministic
partition, not a score bonus.

The core classifies every multi-token query without stable identifier syntax as
`natural_language`. For that class, an `exact_identifier` signal remains on the
candidate and its relevance and source prior still participate in ordinary
scoring, but it does not partition the result set. A project named `clara`, for
example, is a lookup in the one-token query `clara` and a subject in either
`how does clara decide what to remember` or `summarize clara memory`. Stable
identifier syntax — paths, scheme-shaped IDs, sufficiently strong compact IDs,
and underscore project IDs — takes precedence, so both `What is the state of
aaaa0001?` and `What is the state of project_recall?` remain identifier
lookups. In a multi-token identifier query, that precedence is candidate
specific: the exact candidate's candidate ID, source record ID, locator-local
identity, title, or narrowly identity-bearing `name`, `path`, or `relative_path`
metadata must match the stable query token (either as the whole identity or its
path basename). Descriptions, notes, excerpts, and arbitrary metadata are not
identity. Thus the td record partitions for `how does clara address
td-6c98c1?`, but an unrelated exact project-name match for `clara` does not.
Weak version words such as `v2` are not stable identifiers. A declared
multi-word alias is candidate-specific identity rather than request syntax: the
same candidate must carry both `exact_identifier` and `alias`; one cluster
cannot assemble that permission from separate members.

### 6. Optional Shared-Scale Reranking

A reranker may score the fused pool against the query on one shared scale. It
is not required for a useful v1, adds latency, and can mask routing errors. The
pre-rerank ordering and score breakdown are always retained and reported.

### 7. Diversity And Output

Final selection suppresses near-duplicates and preserves source diversity.
Diversity is a selection policy applied after relevance, not a substitute for
it. Output distinguishes high-confidence direct matches, related evidence,
source failures or missing coverage, and no supported recall.

How long the answer is, is two named rules and never an arithmetic accident.
Both are configuration, both appear in the plan, and both count what they
withheld.

`relevance_floor` withholds a result nothing in which is about the query. It is
expressed in `relevance` because that is the only cross-source-comparable
relevance signal fusion may read (§2); a floor on a fused score is forbidden
for the reason abstention is, since those scores are ordinal and uncalibrated.

One floor, two bases. A source may declare that it computes `relevance` on a
basis other than the lexical definition, and the floor still applies to it
unchanged — the value is a number in [0,1] and the rule is a comparison. What the
floor stops meaning is the same thing for every source in the profile: the
declared-basis source's numbers occupy a different range, measured at roughly a
40% discount against lexical scores for one identical passage, so one configured
threshold is simultaneously strict for it and lenient for the others. A profile
mixing bases should expect the semantic source to be withheld first, and a floor
tuned on a lexical profile is not tuned for a mixed one. The core neither detects
nor corrects the difference; the honest reading of a mixed profile's floor is
that it is per-source policy expressed as one number.

It is a selection policy over clusters and not an admission rule over
candidates, and the distinction is load-bearing: a record the caller will not
see still carries information about the records they will — its view of a
record is what tells host suppression the record was already shown, and its
sibling chunk is what representative selection compares against. So the unit is
the result, and the test is that nothing in it clears the floor.

Three rules bound what it may do. An `exact_identifier` match is exempt,
because a record named outright need not describe itself. A candidate reporting
no relevance, or an unusable number, reads as 1.0 and is never withheld by a
rule written in something it did not assert. And **a floor may withhold but
never abstain**: when nothing else would be shown, it withholds nothing,
because an empty answer is read everywhere as a claim about the corpus and a
configured threshold may not make that claim with the corpus unchanged. The
test is against what survived the other selection rules, not against the whole
list, because the rules compose — a floor holding back its one weak result
while suppression removes the strong one has still emptied the answer between
them.

A consequence worth stating, because it looks like a defect from outside: the
rule is **not monotonic in the floor**. Raising a floor can return more results,
since a floor high enough to catch everything catches nothing. The same is true
of the corpus at a fixed floor — deleting the one record above the floor
restores every record below it. Both follow from the exemption, and the
alternative is a threshold that can assert absence.

`max_results` is the profile's result budget, filled in fused order across
every source at once. Without one, an answer's length is the profile's
arithmetic — eligible sources times the per-source cap — so admission decides
WHICH candidates fill each cap and nothing decides how many come back. With
one, adding a source changes which records reach the caller and never how many.

Neither rule is a coverage claim. Every eligible source is still asked and
still reported; what these decide is how much of the answer is shown. The floor
counts what it withheld in `suppressed`, naming each result's lineage root; the
budget counts what it dropped in `truncated` / `dropped`. Neither touches
`coverage`.

Both sit above the response budget, which cuts again and by a different measure
(§Response Budget). A caller whose token budget already binds — a serialized or
tool response at the default ceiling — was receiving a handful of results before
these rules and receives the same handful after; what changed for it is which
records those are. The result count moves where the token budget is loose: the
pointer rendering, and any caller that raises or removes the ceiling.

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
concise human output; for `recall query` the two are tiers of one structure,
under [Output Tiers And Parity](#output-tiers-and-parity).

```text
recall query      search and fuse configured sources
recall expand     retrieve evidence from a locator
recall refresh    update one or all eligible checkpoint-capable sources
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
expand, refresh, and source-status tools, preserving locators and diagnostics rather
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
cmd/recall          CLI entry points
pkg/recall          domain types; depends on nothing else
internal/lineage    locator resolution, lineage roots, independence
internal/config     loading, merge, trust boundary, validation
internal/source     registry, instance resolution, eligibility, plan
internal/app        orchestration: fan out, admit, fuse, shape, decide
pkg/adapter         adapter interface, subprocess supervision
pkg/protocol        JSON-RPC framing, schemas, conformance replay
internal/ranking    grouping, clustering, fusion, selection
internal/evidence   expansion, budget shaping, sanitization
internal/explain    structured explanations and rendering
internal/eval       packs, metrics, gates, runs, reports
internal/api        HTTP and MCP transports
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

[QMD](https://github.com/tobi/qmd) is the second document backend: local BM25,
vector search, hybrid fusion, local reranking, bounded line retrieval, JSON
output. It ships **after** the built-in lexical adapter, as a second
implementation of the same adapter contract, so the benchmark can compare them.
It integrates over a process boundary and never dictates the core language. It
now exists as the optional first-party `recall-qmd` external adapter; see
[qmd-adapter.md](qmd-adapter.md), including what it does not fix.

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

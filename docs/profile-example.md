# Profile Example

A concrete first-deployment inventory. This is configuration, not
specification. Nothing here may leak into core contracts, and the logical
boundaries may change after benchmarking.

Ordering below is the intended integration sequence.

## 1. Clara Project Corpus — documents

**Location:** `~/code/clara-marcus/projects/`

**Records:** Markdown project packets, status notes, decisions, architecture,
open questions, research.

**Authority:** Curated synthesis. Often best for "what is this project?" or
"what did we decide?"; never authoritative for live task state.

**Mode:** indexed, with live expansion from the original file and line range.

**Locator:** repository-relative path, heading or line range, Git revision, and
content hash where useful.

**Retrieval:** lexical first; semantic later, gated by evaluation. Project
directory and document role stay structured metadata.

## 2. Tasks — structured, live

**Location:** `~/code/tasks-marcus/tasks.jsonl` with `archive.jsonl`.

**Records:** current and archived tasks, projects, hierarchy, state, dates,
priority, tags, notes, external links.

**Authority:** authoritative for personal and work task state.

**Mode:** live structured retrieval through the Tasks CLI JSON contract, which
preserves configuration resolution, lifecycle semantics, and stable IDs. An
optional index over titles and bodies may follow.

**Locator:** task ID. Line numbers and title substrings are convenience
references, not locators.

**`as_of`:** `none`. This was written as "filter at best" and that was wrong:
the CLI publishes no creation, revision, or observation timestamp in any JSON
shape. `deadline`, `scheduled`, and `closed` are plan dates, not record
history, so a task created after a boundary is indistinguishable from one that
predates it. Filtering on plan dates would answer a historical question from
current state, which the spec forbids. Declaring `filter` here invited an
adapter to overclaim in exactly the way the manifest field exists to prevent.

## 3. Clara Signals And Observations — the signal store

**Locations:** `~/code/clara-home/data/signals.jsonl`,
`signals-archive.jsonl`, `observations.jsonl`.

**Records:** normalized Jira, Slack, Outlook, Calendar, and Tasks events, plus
explicit user actions such as acting, dismissing, reprioritizing.

**Authority:** signals are a local projection of upstream systems — evidence
with upstream provenance, superseded by live upstream state. Observations are
authoritative for local user behavior.

**Mode:** indexed, rebuilt whole. This was written as "incrementally indexed by
schema version and record cursor" and that premise was wrong. Clara's
`JsonlStore` rewrites each file in place — ingest mutates `last_seen`,
`run_count`, and `lifecycle_state` where the record sits, and the signal
lifecycle moves records into the archive file — so these are not append-only
streams whatever they look like. A byte cursor would miss every rewrite and
keep serving records the corpus has moved or deleted, which
[the index obligations](spec.md#index-obligations) forbid. The adapter reparses
the store whenever a file's size or modification time changes, which is checked
before every search, and a whole rebuild honors deletion for free.

**Locator:** `sig/v<schema>/<record id>`. The schema version is part of the
reference because Clara migrates records between versions in place; the
source-native `ref`, `source_id`, and `occurrence_id` travel in metadata, where
they are searchable as exact identifiers.

**Lineage:** signals projecting upstream records declare `derived_from` using
the source-native identifier. A Tasks record appearing in signals and the
original Tasks record are one lineage root, not independent corroboration. This
is the first source that exercises lineage against real data.

**Observations:** projected onto signals as `last_action`, `last_action_at`, and
`action_count`, and rendered in full by a `context` expansion. They are never
candidates. Clara's own contract already projects `last_action` from
observations at read time rather than persisting it; an observation has no title
and no body, so it would be unfindable by any lexical query; and it would have
to declare its signal's upstream ref as its edge, landing in that signal's
lineage group and displaying nothing.

**`as_of`:** `none`. The records carry `occurred_at`, `created_at`, and
`first_seen`, and none of that is revision history: Clara rewrites a signal on
every ingest, refreshing status, title, and summary from upstream. Selecting by
`occurred_at <= T` would return each record as it is now with a boundary
attached, which is answering a historical question from current state — the same
call, for the same reason, as the Tasks adapter above.

**Sensitivity:** `internal`, matching the sources it projects. A signal about a
task holds the words the task holds, and classifying the projection above the
original would be incoherent when the two collapse into one lineage root.
Individual candidates raise themselves to `confidential` when the signal is
correspondence — Slack, Outlook, Zoom, Calendar — where the excerpt is a
verbatim slice of somebody's message.

## 4. Clara Memory — derived facts

**Location:** `~/code/clara-home/data/memory.jsonl`, with
`memory-archive.jsonl`.

**Records:** distilled facts, feedback, inferred signals, learned preferences,
with stable IDs, subjects, provenance, weight, reinforcement history, and
optional half-life.

**Authority:** derived memory. A useful lead and ranking feature, not
automatically primary evidence about an external system.

**Mode:** indexed, rebuilt whole, for the same reason as the signal store —
`memory consolidate` deletes and `remember` mutates in place. A separate live
path for exact subject lookup was planned and is not needed: the rebuild is
triggered by a stat and the store is small, so an exact lookup already reads
the file as it is now.

**Locator:** `mem/v<schema>/<memory id>`. Subject is a useful dedup key but may
not be globally unique, and the archive can hold several records under one
subject — which is what makes a `context` expansion's subject history useful.

**Lineage:** a generated preference — the `observations-v1` projection Clara
promotes once four distinct signals agree — declares one `derived_from` edge per
ref in its provenance. Several parents makes it a composite: it keeps its own
lineage root and records its ancestors, so corroboration offers it last and
never counts a learned generalization as evidence independent of the signals it
generalizes.

**Decay:** Clara's own `effective = weight × 0.5^(age_days / half_life_days)`,
computed and not reinterpreted, with every input carried into candidate metadata
so a reader can check the arithmetic. `half_life_days: null` means the record
does not decay; `Record::DEFAULT_HALF_LIFE` is a write-time default and is
deliberately not applied at read time, because the records that would reach it
are exactly the ones Clara decided should not decay. Faded records are demoted
by a bounded multiplier and archived records sort below live ones, both still
retrievable, as [the decay rules](spec.md#decay) require. A query carrying a
time window suspends the multiplier entirely. Clara's substring-only retrieval
is not reused anywhere.

**`as_of`:** `none`, for the same reason as the signal store: reinforcement and
consolidation rewrite a memory record in place and leave no prior revision.

**Sensitivity:** `confidential`. Every record is a distilled conclusion about
the owner or the people around him, and an excerpt is the body verbatim. There
is no sub-class of memory that is meaningfully less sensitive, so there is no
per-candidate raise either — inventing one would make the classification depend
on a subject-key convention the corpus does not enforce.

### Worked wiring

Both stores are served by the built-in `clara-corpus` adapter, one instance
each. The compatibility `recall-clara-corpus` command exposes the same adapter
over stdio for older integrations and protocol testing. They are never one
blended source: a signal is evidence about an external system and a memory
record is a durable local conclusion with its own decay, and
[what counts as a source](spec.md#what-counts-as-a-source) follows retrieval
semantics rather than filesystem boundaries.

This goes in the **user** layer. `clara-corpus` is compiled into Recall, so no
`[adapters.clara-corpus]` command declaration is needed or allowed. The source
instances keep the same adapter name and settings as the earlier external
command configuration. This is an example, not the live file.

```toml
# --- Clara signals ----------------------------------------------------------
# What upstream systems have been asking of Marcus, as Clara normalized it,
# with the observation log projected on top. Below `tasks`: a signal is a
# projection and the live task is the authority, and the two collapse into one
# lineage root anyway — the prior decides which of them is shown.

[[sources]]
source_uid = "0000000000000000"   # mint a real one; never hand-edit an existing one
source_id = "clara-signals"
adapter = "clara-corpus"
location = "/Users/example/code/clara-home/data"
location_kind = "path"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.1
record_types = ["task", "message", "event"]

[sources.settings]
store = "signals"
# Clara's civil timezone. Decay and every civil-date conversion depend on it;
# this must match `timezone` in ~/.config/clara/config.
timezone = "America/Los_Angeles"
# Clara source name -> the source_id of the Recall source owning those records.
# This is the lineage mapping, and it is what makes `tasks:d7c7a8a8` on a
# signal identical to the locator the tasks source writes for the same task.
# A Clara source with no entry here emits no edge at all: an invented source_id
# would resolve somewhere, and a wrong lineage root is worse than a missing one.
upstream = { tasks = "tasks" }

# --- Clara memory -----------------------------------------------------------
# What Clara has concluded and kept. High prior for "what do I think about X",
# and confidential, so it is only eligible in a profile that says so.

[[sources]]
source_uid = "0000000000000001"
source_id = "clara-memory"
adapter = "clara-corpus"
location = "/Users/example/code/clara-home/data"
location_kind = "path"
freshness_mode = "indexed"
sensitivity = "confidential"
base_prior = 1.4
record_types = ["memory"]

[sources.settings]
store = "memory"
timezone = "America/Los_Angeles"
# Generated preferences name the observations they rest on; the mapping turns
# those refs into edges, which is what makes the preference a composite.
upstream = { tasks = "tasks" }
```

Two instances of one adapter over one directory is deliberate and safe here:
they open different files, so the `store_identity` diagnostic they publish
differs (`…/data#signals` and `…/data#memory`) and `recall doctor` is content.
Two instances configured over the *same* store — including one at
`~/code/clara-home` and one at `~/code/clara-home/data`, which resolve to one
directory — report one value and are refused, which is the point of the check.

Profile membership needs one deliberate decision. `clara-memory` is
`confidential` and the `home` profile's ceiling is `internal`, so as things
stand it would be reported `skipped` with reason `sensitivity_ceiling` on every
query — correct behavior, and a source that never answers. On a single-user
personal machine the honest fix is to raise the profile, not the floor:

```toml
[profiles.home]
sources = [ "…", "clara-signals", "clara-memory" ]
max_sensitivity = "confidential"
```

The alternative — leaving `home` at `internal` and putting `clara-memory` in a
narrower profile of its own — is also defensible, and is the right shape if
anything else on this machine ever becomes confidential for a different reason.
What is not defensible is lowering the adapter's floor to make the source fit,
which [invariant 4](spec.md#invariants) forbids and which would quietly put
personal memory into every query a guest profile ran.

## 5. td Workspaces — structured, live

**Source of truth:** each td project database associated with a repository.

**Records:** engineering issues, epics, dependencies, review state, comments,
handoffs, sessions, linked files.

**Authority:** authoritative for engineering work in that workspace.

**Mode:** live structured retrieval through the td CLI's JSON surfaces —
`td search` for retrieval and ordering, `td list` for the workspace listing that
supplies the watermark, `td show` for expansion. Recall never depends on td's
private SQLite schema.

**Locator:** workspace identity plus issue ID, as `<workspace>/td-xxxxxx`. td
mints six random hex characters and guarantees them unique only inside one
database, so the workspace is part of the reference and not merely part of the
configuration around it.

**`as_of`:** `none`. td publishes more history than the Tasks CLI —
`created_at`, `updated_at`, `closed_at`, and timestamps on logs, handoffs, and
reviews — and it is still not record history. `updated_at` is a single
last-write stamp with no prior revision behind it, and dependency edges carry
no time at all, so an issue edited after a boundary can only be described as it
is now. Filtering on `created_at` would quietly reinterpret "as the world
looked at T" as "rows created before T". Both are answering a historical
question from current state, which the spec forbids.

**Known gaps in the source contract:** `td search` matches id, title, and
description only, despite help text claiming logs and handoffs, so text living
only in a log or comment is reachable by expansion and not by search. Comments
have no machine-readable CLI surface at all — they exist only over the local
HTTP API, which needs a long-lived writing process — so they are absent from
expansion rather than parsed out of prose. Date comparison in `td query` is
broken upstream and is used nowhere.

Each workspace is a separate source instance behind one td adapter. They are
presented as one source family — one adapter id, one record shape, one locator
grammar — while the workspace boundary survives inside it: sensitivity and
priors are per instance, a project filter routes to the workspace that answers
to that name, and an instance refuses to expand another workspace's locator
rather than returning the issue that happens to share its id.

## Future Structured Databases

Split by record semantics, not one source per database file. Prefer a
documented application API or read-only view. Direct read-only SQL is
appropriate only when the schema itself is a supported contract.

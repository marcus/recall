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

## 3. Clara Signals And Observations — append-only streams

**Locations:** `~/code/clara-marcus/data/signals.jsonl`,
`signals-archive.jsonl`, `observations.jsonl`.

**Records:** normalized Jira, Slack, Outlook, Calendar, and Tasks events, plus
explicit user actions such as acting, dismissing, reprioritizing.

**Authority:** signals are a local projection of upstream systems — evidence
with upstream provenance, superseded by live upstream state. Observations are
authoritative for local user behavior.

**Mode:** incrementally indexed by schema version and record cursor, with
time-window and exact-reference lookup.

**Locator:** record ID plus source-native `ref`, source ID, occurrence ID, and
source revision where present.

**Lineage:** signals projecting upstream records declare `derived_from` using
the source-native `ref`. A Tasks record appearing in signals and the original
Tasks record are one lineage root, not independent corroboration. This is the
first source that exercises lineage against real data.

## 4. Clara Memory — derived facts

**Location:** `~/code/clara-marcus/data/memory.jsonl`

**Records:** distilled facts, feedback, inferred signals, learned preferences,
with stable IDs, subjects, provenance, weight, reinforcement history, and
optional half-life.

**Authority:** derived memory. A useful lead and ranking feature, not
automatically primary evidence about an external system.

**Mode:** live for exact subject lookup; indexed for broader text recall.

**Locator:** schema version plus memory ID. Subject is a useful dedup key but
may not be globally unique.

**Decay:** record-kind-specific decay and recoverable archival live inside this
adapter, per the [decay rules](spec.md#decay). Clara's substring-only retrieval
is not reused anywhere.

## 5. td Workspaces — structured, live

**Source of truth:** each td project database associated with a repository.

**Records:** engineering issues, epics, dependencies, review state, comments,
handoffs, sessions, linked files.

**Authority:** authoritative for engineering work in that workspace.

**Mode:** live structured retrieval through `td export --all --format json`,
`td query`, `td search`, or the local td HTTP API. Recall never depends on td's
private SQLite schema.

**Locator:** workspace identity plus issue ID.

Each workspace is a separate source instance behind one td adapter. They may be
presented as one source family while retaining the workspace boundary for
permissions, routing, and provenance.

## Future Structured Databases

Split by record semantics, not one source per database file. Prefer a
documented application API or read-only view. Direct read-only SQL is
appropriate only when the schema itself is a supported contract.

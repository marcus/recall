# Initial Source Inventory

This inventory is a profile example for the first deployment, not part of the
Recall core. Nothing here may leak into core contracts. The logical boundaries
may change after benchmarking.

## Clara Project Corpus

**Location:** `~/code/clara-marcus/projects/`

**Records:** Markdown project packets, status notes, decisions, architecture,
open questions, and supporting research.

**Authority:** Human- and agent-curated synthesis. Often the best source for
"what is this project?" or "what did we decide?", but not automatically the
authority for live task state.

**Proposed mode:** Indexed, with live expansion from the original file and line
range.

**Stable locator:** Repository-relative path, heading or line range, Git
revision, and content hash where useful.

**Retrieval:** Lexical plus semantic document search. Project directory and
document role should remain structured metadata.

## Clara Memory

**Location:** `~/code/clara-marcus/data/memory.jsonl`

**Records:** Distilled facts, feedback, inferred signals, and learned
preferences with stable IDs, subjects, provenance, weight, reinforcement
history, and optional half-life.

**Authority:** Derived memory. It is a useful lead and ranking feature, not
automatically primary evidence for claims about an external system.

**Proposed mode:** Live for exact subject lookup; indexed or scanned for broader
text recall.

**Stable locator:** Schema version plus memory ID. The source subject is a useful
deduplication key but may not be globally unique.

**Important lesson:** Preserve record-kind-specific decay and recoverable
archive behavior inside this adapter. Do not reuse Clara's substring-only
retrieval as Recall's general ranker.

## Clara Signals And Observations

**Locations:**

- `~/code/clara-marcus/data/signals.jsonl`
- `~/code/clara-marcus/data/signals-archive.jsonl`
- `~/code/clara-marcus/data/observations.jsonl`

**Records:** Normalized Jira, Slack, Outlook, Calendar, and Tasks events plus
explicit user actions such as acting, dismissing, or reprioritizing.

**Authority:** A normalized local projection of upstream systems. Signals are
evidence with upstream provenance, but live upstream state may supersede them.
Observations are authoritative evidence of local user behavior.

**Proposed mode:** Incrementally indexed by schema version and record cursor,
with time-window and exact-reference lookup.

**Stable locator:** Record ID plus source-native `ref`, source ID, occurrence ID,
and source revision where present.

**Lineage:** Signals that project upstream records declare `derived_from`
locators using the source-native `ref`. A Tasks record appearing in Clara
Signals and the original Tasks record are one evidence lineage, not independent
corroboration.

## Tasks

**Source of truth:** The configured Tasks corpus, currently
`~/code/tasks-marcus/tasks.jsonl` with `archive.jsonl`.

**Records:** Current and archived tasks, projects, hierarchy, state, dates,
priority, tags, notes, and external links.

**Authority:** Authoritative for Marcus's personal and work task state.

**Proposed mode:** Live structured retrieval through the Tasks CLI JSON
contract, with an optional index for semantic retrieval over titles and bodies.
Using the CLI preserves configuration resolution, lifecycle semantics, and
stable IDs.

**Stable locator:** Task ID. Line numbers and title substrings are convenience
references, not stable locators.

## td Workspaces

**Source of truth:** Each td project database associated with a repository.

**Records:** Engineering issues, epics, dependencies, review state, comments,
handoffs, sessions, and linked files.

**Authority:** Authoritative for engineering work tracked in that td workspace.

**Proposed mode:** Live structured retrieval through `td export --all
--format json`, `td query`, `td search`, or the local td HTTP API. Recall should
not depend on td's private SQLite schema.

**Stable locator:** Workspace identity plus issue ID.

Each workspace is a source instance behind one td adapter. Recall may present
them as one source family while retaining the workspace boundary for
permissions, routing, and provenance.

## Additional Structured Databases

Future SQLite sources should be split into logical adapters by record semantics,
not registered as one source per database file. An adapter should prefer a
documented application API or read-only view when available. Direct read-only
SQL is appropriate when the schema itself is a supported contract.

// Package td is Recall's built-in adapter for td workspaces: a live,
// structured source over the engineering issues of one repository.
//
// # One adapter, one instance per workspace
//
// td resolves its database by repository: every repo has its own workspace,
// its own issue ids, and its own answer to "what is open". So one configured
// source instance is one workspace, and several instances share this adapter
// the way several projects share the documents adapter. They present as one
// source family — same adapter id, same record shape, same locator grammar —
// while the workspace boundary survives inside it, because that boundary is
// what permissions, routing, and provenance are about:
//
//   - Permissions. Sensitivity, priors, and profile membership are configured
//     per instance, so a work workspace can be excluded from a personal
//     profile without excluding td.
//   - Routing. A request filtered to a project reaches only the instance whose
//     workspace answers to that name; see [Adapter.Search].
//   - Provenance. Every locator, every candidate's metadata, and every
//     expansion names the workspace it came from, so two issues that merely
//     share an id shape are never confused for one record.
//
// The workspace name is the locator's own namespace rather than the source id,
// because a source id is renameable configuration and a locator has to survive
// re-runs and renames. A locator reads `<source_id>:<workspace>/td-369eef`.
// Expanding a locator that names a different workspace is refused, not
// answered from this one.
//
// # Identity comes from the database, not from the configuration
//
// The name is the base name of the directory td RESOLVES the location to, and
// never of the location itself. The difference is not cosmetic: td walks
// upward to find its database, so a repository and any subdirectory, worktree,
// or submodule of it all open one SQLite file. Taking identity from the
// configured path made two such instances `recall` and `docs`, and because
// lineage groups on source_uid plus source_record_id, one issue in one file
// arrived as two independent pieces of evidence and was scored up for
// corroborating itself — while `recall doctor` reported ok. It failed the other
// way too: an instance would refuse a locator naming an issue held in the
// database it had itself opened.
//
// Three things hold the identity to the database now. [resolveRoot] resolves
// the root the way td does and [Adapter.Health] checks its answer against the
// project name `td info` reports, degrading the source when they disagree. The
// `workspace` setting asserts an identity rather than overriding one, so no
// configured string can rename another workspace's database. And every
// candidate carries a content fingerprint over the issue's own identity and
// version, so even a duplicate configuration cannot corroborate itself — which
// is what makes the failure recoverable rather than silent.
//
// # Boundary
//
// Everything goes through the `td` executable and nothing reads
// `.todos/issues.db`. That database is td's private schema, and the spec
// permits direct SQL only where the schema itself is a supported contract.
// The CLI owns workspace resolution (`.td-root`, git roots, worktrees),
// lifecycle semantics, and id minting; reimplementing any of it here would
// mean disagreeing with td eventually. Only the read-only subcommands in
// [readOnlyCommands] may be invoked, and [checkReadOnly] is the gate.
//
// [resolveRoot] is the one deliberate exception, and it is an exception in
// form only: it mirrors td's resolution to learn WHICH database td will open,
// never to open one, and nothing trusts its answer — td's own project name is
// what confirms it on every probe. A mirror that drifts produces a source
// reporting that it cannot confirm its identity, rather than one quietly
// answering for the wrong workspace.
//
// The adapter owns no index: every search reads the workspace, which is why
// the manifest declares only [recall.FreshnessLive].
//
// # Retrieval
//
// Ordering is delegated to the source. `td search` scores id, title, and
// description matches into fixed buckets and returns them ranked, and that
// ranking is what this adapter emits for a single-probe query; it does not
// re-score what td already ordered.
//
// td's matching is substring-only, so a two-word query matches only text
// containing those two words adjacently, and a query that is a sentence
// matches nothing at all. Delegating the whole query verbatim would therefore
// return nothing for most natural questions. The adapter issues one probe per
// query term instead, capped, and merges the answers:
//
//  1. Exact id hits first, in the order the ids appear in the query. A
//     partition, not a bonus, mirroring the core's exact-match promotion.
//  2. Then by how many distinct query terms found the issue, which is the one
//     ranking judgment this adapter adds and the one td cannot make, since
//     each probe only knows about its own term.
//  3. Then by td's own score, then priority, then most recently updated, then
//     id, so the order is total and reproducible.
//
// # Known gaps in the td contract
//
// These are limits of the documented surfaces, not of this adapter, and each
// one is reported rather than papered over:
//
//   - `td search` matches id, title, and description only. Its help text
//     claims logs and handoffs; the SQL does not. Text that exists only in a
//     log or a handoff is unfindable by search, though expansion returns it.
//     Every search says so in its diagnostics under `search_scope`.
//   - Comments have no machine-readable surface. `td export` omits them,
//     `td show --json` omits them, and `td comments` prints minute-precision
//     prose with no ids and no escaping. They are available only from the
//     local HTTP API, which requires running `td serve` — a long-lived,
//     writing process a read-only adapter has no business starting. So
//     comments are absent from expansion, and that is stated here rather than
//     approximated by parsing prose.
//   - Date comparison in `td query` is broken upstream: `created < X` matches
//     nothing and `created > X` matches everything, silently. Nothing here
//     uses it. `td list --updated` works but only at day granularity.
//   - `td list --sort` did not order by the requested field in the version
//     this was built against, so nothing here depends on td's ordering except
//     `td search`, whose scoring is explicit in its output.
//   - Every td invocation, including read-only ones, touches the workspace:
//     it appends to `.todos/command_usage.jsonl` and upserts a session row.
//     A search is read-only in intent and in effect on issue data, but it is
//     not a no-op on the workspace directory. That is td's behavior, not
//     something this adapter can opt out of through a supported surface.
//
// # Conformance
//
// The transcripts in conformance/ are replayed against this adapter over the
// real wire protocol, through [adapter.Serve], by the same engine that drives
// an external adapter binary. Each case's fixture is recorded td output
// answered through the `replay` setting, so a transcript proves the parser,
// the merge, and the error mapping without depending on a workspace that
// changes with every commit.
//
// # as_of
//
// [recall.AsOfNone].
//
// td publishes real record history in a way the Tasks CLI does not:
// `created_at`, `updated_at`, and `closed_at` are per-record timestamps, and
// logs, handoffs, and reviews each carry their own. It is tempting to declare
// `filter` on that evidence. It would be wrong.
//
// `updated_at` is a single last-write timestamp, not a revision history. There
// is no per-field change record, no prior revision of a title or a
// description, and dependency edges carry no timestamp at all. So an issue
// that existed before the boundary and was edited after it can be reported
// only as it is now — which is answering a historical question from current
// state, the one thing docs/spec.md forbids outright. Filtering on
// `created_at` would be worse: it silently reinterprets "as the world looked
// at T" as "rows created before T", quietly dropping every issue that existed
// then and has been touched since, and quietly returning present-day content
// for the rest.
//
// `snapshot` is unavailable for the same reason: no public td surface can
// reconstruct an issue's state at a past instant. `none` is what this source
// can actually keep, and the manifest field exists precisely to stop an
// adapter claiming more.
package td

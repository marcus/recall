// Package tasks is Recall's built-in adapter for the Tasks CLI: a live,
// structured source over a personal GTD store.
//
// # Boundary
//
// The adapter owns retrieval, ranking within this source, and locator
// semantics. It owns no index: every search reads the store of truth through
// the CLI, which is why the manifest declares only [recall.FreshnessLive].
//
// Everything goes through the `tasks` executable and nothing reads
// tasks.jsonl. The CLI owns configuration resolution, lifecycle semantics
// (availability, deferral, recurrence, cascading completion), and stable id
// minting. Parsing the file would couple Recall to a private on-disk format
// and would reimplement three sets of rules that already have one owner.
//
// The CLI is a one-shot command, not a server, so a search costs one process
// spawn per invocation. Invocations are issued concurrently and both the wall
// time and the summed process time are reported in search diagnostics, so the
// cost of this design is measured rather than assumed. Only the read-only
// subcommands in [readOnlyCommands] may ever be invoked; see [checkReadOnly].
//
// # Ranking
//
// Local ranking is deliberately simple and explainable:
//
//  1. Exact identifier hits first, in the order their ids appear in the query.
//     This is a partition, not a bonus, mirroring the core's own exact-match
//     promotion.
//  2. Everything else by a weighted term-coverage score over the title
//     (word-boundary 1.0, substring 0.6), the record's own fields — tags,
//     contexts, state, project (0.5) — and the body (0.4).
//  3. Ties broken by soonest date, then priority, then id, so the order is
//     total and reproducible.
//
// Body text is not in the CLI's bulk list shape, so body relevance is obtained
// by re-asking the CLI with `--body` once per query term (capped): the extra
// ids it returns are the body-only matches. That buys body recall for a
// bounded number of spawns instead of one spawn per candidate.
//
// # Known gaps in the CLI contract
//
// The bulk task shape carries no project, so project metadata comes from
// `tasks projects --json`, which rolls up over open, non-deferred tasks filed
// under a project or area. Inbox items and closed work appear in no rollup and
// therefore carry no project at search time; expansion reads the true value
// from `tasks show`. A search that filters on project says so in its
// diagnostics rather than letting the gap look like an empty result.
//
// # as_of
//
// [recall.AsOfNone]. docs/profile-example.md hopes for `filter`, but the CLI
// exposes no creation, revision, or observation timestamp in any JSON shape:
// `deadline`, `scheduled`, and `closed` are plan dates, not record history. A
// task created after a boundary is indistinguishable from one that predates
// it, and a retitled task shows only its current title. Filtering on plan
// dates would answer a historical question from current state, which
// docs/spec.md forbids, so the boundary is refused instead.
package tasks

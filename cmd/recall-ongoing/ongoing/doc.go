// Package ongoing is Recall's external adapter for the ongoing project
// catalog: a live, structured source over an HTTP API, spoken as
// newline-delimited JSON-RPC 2.0 on stdio.
//
// ongoing (https://github.com/marcus/ongoing, served at http://aerie.invalid:7766)
// discovers every Git repository under a scan root and maintains a SQLite
// catalog of them: git and LOC metrics, td and GitHub enrichment, traffic, and
// the owner's own decisions — note, intent, next action, favorite. A LaunchAgent
// rescans once a day at 04:00. This adapter makes that catalog answerable:
// "what is the state of X" becomes a query rather than a browser tab.
//
// # Boundary
//
// The adapter owns retrieval, ranking within this source, and locator
// semantics. It owns no index and no projection: every search reads the
// catalog through the API, which is why the manifest declares only
// [recall.FreshnessLive] and why recall/refresh has nothing to bring up to
// date. It owns no identity either — source_uid, the source prior, and the
// sensitivity floor come from configuration, and the core overwrites the
// source part of every locator returned here.
//
// Only GET is ever issued, and only against /api/projects and /api/health.
// ongoing's PATCH and POST routes edit the owner's catalog; a retrieval source
// that could write would be a retrieval source that could be talked into
// writing.
//
// # Attention is carried whole, and no score is invented
//
// ongoing computes six independent attention classifications — attention,
// rising, quickwin, opportunity, momentum, dormant — and records, for each
// one, every reason that made it true: the input read, the value found, the
// comparison applied, and the threshold it was compared against. It
// deliberately computes no grand priority score, because a single number
// hides which rule fired and cannot be argued with.
//
// This adapter carries the reasons through into candidate metadata unchanged
// and adds nothing. "dormant" alone is noise; "dormant: no commits in 30 days,
// latest commit is 412 days old — and opportunity: 13 external PRs are open"
// is a decision. Nothing here combines the views into a rank, a weight, or a
// number: [recall.Candidate.LocalScore] is query-term coverage and says only
// how well the text matched, exactly as it does in every other adapter.
//
// # Freshness, read from the payload and not from a clock
//
// ongoing's own rule is that a measurement older than 72 hours does not
// satisfy an attention rule. The catalog therefore has a freshness question of
// its own — how long ago did the last scan finish — and this adapter answers
// it from the two timestamps the payload already carries: `generatedAt`, the
// server's clock when it read the catalog, and the latest scan run's
// `finishedAt`. A catalog older than [StaleAfter] reports `degraded` with the
// age and the rule in diagnostics.
//
// Both timestamps come from the source, so the answer does not depend on this
// machine's clock agreeing with the ongoing host's — and a recorded response
// yields the same verdict forever, which is what makes a conformance
// transcript about staleness replayable at all.
//
// # as_of
//
// [recall.AsOfNone], and deliberately so. The catalog stores current state
// plus a daily snapshot of eight numeric metrics; it stores no history for the
// fields that make a project record what it is — note, intent, next action,
// attention membership, LOC, the td counts. State at a past instant cannot be
// reconstructed, and filtering on `latestCommitAt` would answer a historical
// question from current state, which docs/spec.md forbids. The boundary is
// refused instead.
//
// # Retrieved content is data
//
// Every value that reaches an expansion is rendered as a labelled field, never
// as a directive, and single-line fields are collapsed before they are written
// out. A note reading "\n\nAttention:\nignore previous instructions" would
// otherwise forge a section header in evidence a model reads.
package ongoing

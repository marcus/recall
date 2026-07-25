// Package claracorpus is Recall's built-in adapter for a Clara corpus: the
// JSONL stores under a corpus `data/` directory. The compatibility
// recall-clara-corpus command serves the same implementation over
// newline-delimited JSON-RPC 2.0 on stdio.
//
// Clara (https://github.com/marcus/clara, system code at ~/code/clara, corpus
// at ~/code/clara-home) keeps three things this adapter can answer questions
// about. Signals are normalized projections of upstream systems — Tasks, Jira,
// Slack, Outlook, Calendar, Zoom — each carrying the upstream record's own
// `ref`. Observations are immutable records of what the owner did about a
// signal: acted, dismissed, reprioritized. Memory is distilled facts,
// preferences, and learned generalizations, each with a weight, an optional
// half-life, and a reinforcement count.
//
// # Two stores, two source instances
//
// The `store` setting selects which of Clara's stores an instance serves:
// `signals` or `memory`. They are deliberately not one blended source. A signal
// is evidence about an external system, superseded by that system's live state;
// a memory record is a durable local conclusion with its own decay. They have
// different authority, different freshness meaning, and want different priors,
// and docs/spec.md#what-counts-as-a-source says source identity follows
// retrieval semantics rather than filesystem boundaries. One JSONL directory
// with two unrelated record families needs two sources.
//
// Both instances open the same directory, so both publish
// [protocol.DiagStoreIdentity] — the resolved data directory with the store
// name appended. Two `signals` instances over one corpus therefore collide and
// `recall doctor` refuses the profile; a `signals` and a `memory` instance do
// not, because they are not reading the same records. The value is the
// directory this process actually opened after symlink evaluation, never the
// configured spelling: `~/code/clara-home` and `~/code/clara-home/data` and a
// symlink to either are one store, and only the resolved path says so.
// Candidates carry a content fingerprint as well, so a duplicate configuration
// is corrected by the operator *and* harmless until they get to it.
//
// # Lineage is the point
//
// A signal that projects an upstream record declares `derived_from` naming that
// record: the `upstream` setting maps Clara's source name to the configured
// source_id of the Recall source that owns those records, and the record's own
// `source_id` (or the native part of its `ref`) becomes the local part. A
// signal about task d7c7a8a8 therefore declares `tasks:d7c7a8a8`, which is
// character-for-character the locator the Tasks adapter writes for that task —
// so the signal and the task collapse into ONE lineage root and never
// corroborate each other. An unmapped Clara source emits no edge at all: an
// invented source_id would resolve somewhere, and a wrong lineage root is worse
// than a missing one.
//
// A generated preference — the `observations-v1` projection Clara promotes once
// four distinct signals agree — declares one edge per ref in its provenance. It
// is a COMPOSITE, and the core treats it as one: several parents means it keeps
// its own lineage root, so it is still displayable, while recording its
// ancestors so corroboration offers it last and never counts a learned
// generalization as evidence independent of the signals it generalizes. That is
// the whole reason `derived_from` accepts a list.
//
// # Observations are a projection, not candidates
//
// An observation never becomes a candidate. Three reasons, in the order they
// settle it.
//
// Clara's own contract already says so: `last_action` is not persisted in
// signals.jsonl, and Signals::Store projects it from observations.jsonl at read
// time. Reusing that projection keeps one truth about what the owner did rather
// than minting a second.
//
// An observation has no title and no body. It is (ref, action, occurred_at) and
// nothing else, so as a candidate it would be unfindable by any lexical query
// and reachable only by the ref its signal already answers to.
//
// And it would collapse anyway. An observation is about its signal, so it would
// have to declare that signal's upstream ref as its edge, landing in the same
// lineage group and losing to the signal on selection — costing an edge and a
// fingerprint to display nothing.
//
// So observations arrive where a reader can use them: `last_action`,
// `last_action_at`, and `action_count` in a signal's candidate metadata, and
// the full reaction history in a `context` expansion, each with its own
// occurred_at. A parse failure in observations.jsonl makes a signal's action
// state unknown, and the search reports `partial` for it — a signal shown
// without an action it actually has is a false absence, which is the failure
// this system exists to prevent. An observations.jsonl that does not exist is
// not that: Clara creates the file on first observation, so its absence means
// nothing has been acted on, and reporting that as partial would make a
// correctly configured corpus permanently degraded.
//
// # Decay is Clara's, carried through with its reasons
//
// docs/spec.md#decay puts decay inside the adapter of a source that has it, and
// Clara has it: effective = weight × 0.5^(age_days / half_life_days), aged from
// `last_seen`, with `half_life_days: null` meaning a record does not decay at
// all. This adapter computes that expression and no other. Every input travels
// with the answer — weight, half_life_days, age_days, the field the age was
// measured from, and the formula itself — so a reader can check the arithmetic
// instead of trusting it.
//
// Two temptations are refused. Clara's Record::DEFAULT_HALF_LIFE (preference
// 45, signal 10) is a WRITE-time default: it picks a half-life when a record is
// created and is never consulted again. Applying it at read time to a record
// whose `half_life_days` is null would be a second decay model contradicting
// the first, and the records that reach it are exactly the ones Clara decided
// should not decay. And signals get no age arithmetic here whatsoever: their
// fading is Clara's lifecycle — terminal status, per-source expiry,
// archival — already decided and already written down. Recomputing an expiry
// from `last_seen` would be this adapter second-guessing the corpus with a rule
// the corpus does not hold.
//
// Faded material stays retrievable, as the spec requires. A decayed memory
// record is demoted by a bounded multiplier — at worst it ranks as though its
// text matched half as well, never as though it had not matched — and archived
// records sort below live ones rather than out of the result. A query carrying
// an explicit time window suspends the decay multiplier entirely: a historical
// question retrieves old evidence without a recency penalty, and the records
// were already selected by the window.
//
// # Freshness: rebuilt whole, because these files are rewritten
//
// docs/profile-example.md describes these stores as append-only streams
// indexed by record cursor. The implementation says otherwise, and the
// implementation wins: Clara's JsonlStore rewrites each file in place —
// `memory consolidate` deletes records, `signals archive` moves them to another
// file, and reinforcement mutates a record's weight and last_seen where it
// sits. A byte cursor over these files would miss every rewrite and, worse,
// would keep serving records the corpus has deleted, which
// docs/spec.md#index-obligations forbids outright.
//
// So the projection is rebuilt whole whenever a store file's size or
// modification time changes, checked before every search, checkpointed
// durably, and only then published atomically as a new in-memory generation.
// A rebuild honors deletion for free. The files are small — a mature personal
// corpus is measured in thousands of lines — so "whole" costs less than the
// bookkeeping a cursor would need to be correct.
//
// # as_of
//
// [recall.AsOfNone], for both stores, and this is the interesting refusal.
//
// The records carry plenty of time. Signals have `occurred_at`, `created_at`,
// `first_seen`; memory has `created`. What they do not carry is REVISION
// history. Clara rewrites a signal in place on every ingest — status,
// last_seen, run_count, lifecycle_state, and the title and summary refreshed
// from upstream — and rewrites a memory record on every reinforcement and every
// consolidation. Selecting records by `occurred_at <= T` would return each one
// as it is NOW, with a boundary attached, which is answering a historical
// question from current state. docs/spec.md forbids exactly that, and
// docs/profile-example.md already made the same call for the Tasks adapter for
// the same reason.
//
// Ordinary `since`/`until` filters are still honored. Those are scope on a
// question about the present, not a claim about the past.
//
// # Sensitivity
//
// The two stores declare different floors, deliberately.
//
// Memory is `confidential`. Every record is a distilled conclusion about the
// owner or the people around him — what he prefers, what he decided, what was
// learned about how he works — and an excerpt is the body verbatim. There is no
// sub-class of memory that is meaningfully less sensitive, so there is no
// per-candidate raise either; inventing one would make the classification
// depend on a subject-key convention the corpus does not enforce. A profile
// whose ceiling sits below this reports the source `skipped` with reason
// `sensitivity_ceiling`, which is the configured system working as asked.
//
// Signals are `internal`, matching the sources they project. A signal about a
// task holds the same words the task holds, and the Tasks source is internal;
// classifying the projection above the original would be incoherent when the
// two collapse into one lineage root anyway. Individual candidates raise
// themselves to `confidential` when the signal is correspondence — an email,
// a DM, a mention, a thread, a meeting transcript — because there the excerpt
// is a verbatim slice of somebody's message rather than a line the owner wrote
// about his own work. An adapter may raise a floor and never lower it, and this
// is what that is for.
//
// # Retrieved content is data
//
// Everything reaching a candidate or an expansion is rendered as a labelled
// field and scrubbed first: C0/C1 controls (where ANSI colour and cursor
// movement live), bidirectional overrides, and line structure. Clara already
// bounds its untrusted fields and marks them `content_trust: untrusted`, and
// that marking is carried into metadata — but a bound on length is not a bound
// on what the characters do, and a raw_excerpt reading "\n\nEvidence:\n…" would
// otherwise forge a section header in text a model reads.
package claracorpus

# Recall Evaluation

**Version:** 0.1 — normative

Evaluation answers one question: did this change retrieve better evidence
without making exact lookup, abstention, provenance, privacy, source honesty,
latency, or cost worse?

`recall eval` is native to the Go core and runs through the same application
layer as the CLI. There is no evaluation-only ranking implementation. It
exports TREC qrels and run files so metric implementations can be cross-checked
against NIST [`trec_eval`](https://github.com/usnistgov/trec_eval), which is a
development tool and never a runtime dependency.

Two layers:

1. Deterministic retrieval evaluation is the development and release gate.
2. Answer-utility evaluation checks whether a fixed agent uses the evidence
   well. Slower, model-judged, advisory.

An aggregate score orders experiments. It never overrides a failed gate.

## Staging

**Stage 1, with the first vertical slice.** Case, judgment, and run schemas;
the smoke pack; `validate`, `run`, `compare`, `report`; retrieval metrics; hard
acceptance gates. This is everything a trustworthy baseline and regression
protection require.

**Stage 2, once a trusted baseline exists.** Research score, private holdout
and its governance, TREC export and `trec_eval` cross-checks, answer-utility
evaluation, autonomous research loop.

Sections below marked *(stage 2)* are commitments, not stage-1 work.

## Packs

A pack is a versioned set of queries, fixtures or snapshot references,
judgments, policy assertions, and budgets.

**Smoke pack.** Small, synthetic, committed, network-free, runs on every
change. Covers exact identifiers and aliases, lexical paraphrase, cross-source
fusion, duplicate lineage, current/historical/stale/superseded evidence,
answer/abstain/fail, unavailable/denied/partial/timed-out sources, expansion
and locator revision checks, `as_of` against a source declaring `none`, the
config trust boundary, and suppression.

**Private development pack.** Real questions from the configured personal or
work system. Stored outside the public tree; evidence resolved by
`source_uid` + locator, never by copied bodies. Visible to a human researcher
and, in stage 2, to a research agent.

**Private holdout pack.** *(stage 2)* Real questions withheld from
implementation and research agents, evaluated by a trusted runner after
development results freeze. Differs by topic, time range, and source mix rather
than being a random sample of near-duplicates.

**Live health pack.** Small read-only check against configured live sources.
Measures operational drift. Live records change, so its results are never the
optimization score.

## Layout

```text
eval/
  schema/       pack, case, judgment, run schemas, plus embed.go
  packs/smoke/  pack.json, cases.jsonl, judgments.jsonl, sources/, transcripts/
  baselines/    smoke.json
```

Private packs live elsewhere and are selected by absolute path or profile. The
repository holds schemas, synthetic fixtures, and public baselines — never
personal or employer source data.

Run artifacts contain excerpts even when packs do not. They inherit the pack's
sensitivity, default to `$XDG_STATE_HOME/recall/<profile>/eval/`, and are never
committed.

## Cases And Judgments

Queries and judgments are separate files so a runner can give an agent the
queries while withholding the answers.

```json
{
  "schema_version": 1,
  "case_id": "tasks-exact-001",
  "query": "What is the state of td-f62256?",
  "profile": "smoke",
  "as_of": "2026-07-23T12:00:00-07:00",
  "expected_behavior": "answer",
  "tags": ["exact", "task", "current-state"],
  "timeout_ms": 250,
  "notes": "Exact stable identifier must reach the top of the fused list."
}
```

A judgment targets an evidence lineage, not rendered text:

```json
{
  "schema_version": 1,
  "case_id": "tasks-exact-001",
  "lineage_root": "01J8ZK…:td-f62256",
  "relevance": 3,
  "required": true,
  "forbidden": false,
  "supports": ["state", "title"]
}
```

`lineage_root` keys on `source_uid`, so renaming a source does not invalidate a
pack.

```text
relevance  0 irrelevant | 1 related context | 2 useful support | 3 authoritative
behavior   answer | abstain | fail
```

The behavior vocabulary matches `outcome` one for one. An earlier draft had a
`clarify` behavior, which no outcome could produce — a case expecting it could
never pass. Recall retrieves; deciding to ask a follow-up question is something
a host does with what Recall returned, and it belongs to answer-utility
evaluation.

A case may also declare policy assertions in an `assertions` block. These are
the ground truth for the policy metrics, which never read them back off the
response — one source of truth, or the metric would grade the system against
itself:

```text
expected_coverage: complete | degraded
expected source outcome per source
required or forbidden source
required abstention
maximum latency
maximum expansion size
locator must resolve to the judged lineage and revision
candidate must be suppressed or visible
no candidate may cross the profile sensitivity ceiling
```

## Reproducible Execution

Every deterministic run records: Recall commit and dirty-tree state; pack
identity and content hash; resolved profile hash with secrets removed; adapter
and index versions; model names and artifact hashes; host OS, architecture, and
memory; cache policy and warm/cold state; random seeds; and per-case source
outcomes, candidates, lineage roots, explanations, and timing.

Fixture runs inject the clock from each case's `as_of`. Network access is
disabled. Model-backed and network-backed adapters replay recorded protocol
transcripts (see [conformance](adapter-protocol.md#conformance)) rather than
running live.

```text
recall eval validate --pack <path>
recall eval run      --pack <path> --output <dir>
recall eval compare  <baseline> <candidate>
recall eval report   <run>
recall eval export-trec <run>          (stage 2)
```

A run produces `run.json` (environment, metrics, gates, status),
`cases.jsonl` (one detailed result per case), `summary.md`, and
`results.trec` when applicable.

## Metrics

| Metric | Use |
| --- | --- |
| nDCG@10 | Graded ranking quality near the top |
| Recall@5, Recall@20 | Whether required evidence was retrieved at all |
| MRR@10 | Rank of the first authoritative result, especially exact queries |
| Success@5 | Fraction of cases with useful evidence near the top |
| Forbidden@5 | Rate of forbidden or superseded evidence near the top |
| Abstention accuracy | Correct answer, abstain, or fail behavior |
| Coverage accuracy | Degraded coverage reported when and only when true |
| Locator success | Returned references that expand to the judged revision |
| Provenance accuracy | Correct source and lineage root per candidate |
| Source-outcome accuracy | Failure, denial, timeout, and partial reported honestly |
| p50 / p95 latency | Typical and tail response time, cold and warm |
| Query cost | Model calls, tokens, external requests — lands with the runner, once there is a cost to count |

### Definitions

Two implementations can both be called "nDCG@10" and disagree by several
points, so the formulations are fixed here rather than left to the code:

```text
nDCG@k        linear gain, rel_i / log2(i+1). Not 2^rel - 1.
Recall@k      denominator is the judgments marked required, not every
              positively graded one.
MRR@10        target is grade 3 (authoritative) only.
Success@5     threshold is grade >= 2 (useful support).
Forbidden@5   any judgment marked forbidden appearing in the top 5.
percentiles   nearest-rank.
```

**An undefined metric is not zero.** A case with nothing to find has no recall,
a pack that puts nothing at stake has no Forbidden@5, and a case with no policy
assertion has no coverage accuracy. Every per-case metric reports whether it
applies and every average carries its `N`. Scoring an undefined metric as zero
would manufacture regressions and let an abstention-heavy pack drive
Forbidden@5 to a flattering zero.

### Grouping

Metrics are reported per case tag and per source family, each with its own
macro average across that dimension's groups. One large document source must
not hide regressions in exact lookups, temporal questions, or no-answer cases.

The research score in stage 2 uses the **case-tag** macro. Tags are how a pack
declares what a case is testing, so they are the dimension a regression should
be visible along.

Latency has no macro average. A percentile of percentiles is not a percentile;
latency is pooled within each population and reported per group and overall.

Metric implementations are verified against published worked examples, term by
term so the formulation is pinned and not only the arithmetic, and in stage 2
against `trec_eval`. Accuracy metrics with no published vector — abstention,
coverage, locator, provenance, source outcome — are verified against explicitly
constructed cases and are not claimed as published.

## Acceptance Gates

A candidate run is invalid when any hard gate fails:

- Zero sensitivity-ceiling violations.
- Zero forbidden results for security-critical cases.
- Every returned locator resolves to the judged lineage and revision.
- Source failure, denial, timeout, and partial-coverage fixtures report the
  expected outcome and coverage.
- Exact-identifier Success@1 meets the pack threshold.
- Abstention accuracy meets the pack threshold.
- p95 latency and query cost stay within the pack budget.
- The run completes with no fixture mutation, crash, or undeclared network use.
- No project-configuration fixture caused a subprocess spawn.

On the first run there is no baseline, so thresholds are absolute values
recorded in the pack. After the first baseline they become
"does not regress beyond run variance", set from measured distributions — never
copied from another retrieval system.

## Research Score *(stage 2)*

Runs passing every gate receive a development score:

```text
research_score =
    0.50 * macro_nDCG@10
  + 0.25 * macro_Recall@5
  + 0.15 * macro_MRR@10
  + 0.10 * macro_Success@5
```

Weights are provisional until baseline distributions exist. The score orders
experiments. Release also requires: no meaningful regression in any case group;
improvement larger than run variance; a paired per-case comparison with a
confidence interval; review of newly failing and newly passing cases; and a
complexity check over code size, dependencies, and operational cost. When
quality is equal, simpler code wins.

## Answer Utility *(stage 2)*

A separate suite sends the same query and Recall output to a fixed answer model
and prompt, measuring supported-claim precision and recall, citation
correctness, unsupported-claim rate, completeness, correct abstention, and
prompt cost. Model, prompt, temperature, provider, and rubric are versioned. A
model judge never creates ground truth silently; human-reviewed examples anchor
the rubric and disagreements stay visible.

## Autonomous Research *(stage 2)*

Fixed harness, narrow edit surface, baseline first, one hypothesis per
experiment, machine-readable keep/discard log.

**Immutable during a run:** runner and metric implementations; pack schemas,
fixtures, cases, judgments; the holdout; the dependency lockfile; time, cost,
cache, and hardware policy; permission and provenance gates. The runner
verifies hashes before and after every experiment.

**Editable surface:** declared per campaign. A first campaign allows
`internal/ranking/`, `internal/source/` eligibility rules, and research config.
Adapters, public contracts, evaluation code, and test data are out of scope.

**Experiment record**, appended to `experiments.jsonl`:

```json
{
  "experiment_id": "20260723-014",
  "parent_commit": "abc1234",
  "candidate_commit": "def5678",
  "hypothesis": "Lower the RRF constant for clusters with an exact match.",
  "changed_paths": ["internal/ranking/fuse.go"],
  "status": "keep",
  "research_score_before": 0.7124,
  "research_score_after": 0.7191,
  "gates": "pass",
  "p95_ms_before": 41.2,
  "p95_ms_after": 40.9,
  "description": "Improved exact and task groups without regressions."
}
```

**Loop:** isolated worktree → validate pack and environment hashes → record the
unchanged baseline → one falsifiable hypothesis → change only allowed files and
commit → run the development pack inside the experiment budget → discard
crashes, gate failures, and non-improvements → keep a passing improvement as
the next baseline → stop at the campaign budget → freeze the best candidate for
holdout evaluation by a trusted runner.

The loop cannot merge to `main`, read or alter the holdout, raise its own
budget, add dependencies, enable network access, or publish results. A human
reviews candidate, holdout report, and complexity before promotion.

An agent may discover a better ranking policy. It may not encode case IDs,
expected locators, query strings, or source-specific answers into production
logic.

**Against overfitting:** split by topic, time, source family, and query form;
keep paraphrases in the same split; add production failures as regression cases
and rotate related holdout cases; inspect per-group changes rather than the
aggregate; limit experiments per campaign and log every hypothesis; re-run
finalists from a clean checkout; require gains to survive an alternate pack or
time slice.

## First Milestone

Delivered alongside the first vertical slice, in this order:

1. Case, judgment, run, and pack schemas.
2. 30–50 synthetic smoke cases covering every category above.
3. `recall eval validate` and `run` against the smoke pack.
4. 15–25 real development cases across the two slice sources.
5. The lexical, unweighted-RRF baseline.
6. Recorded metric and latency distributions on the target machine, cold and
   warm.
7. Gate thresholds and a minimum meaningful improvement, derived from 6.

The goal is a trustworthy baseline, not an overnight research loop. The
holdout, research score, and autonomous loop begin only after step 7.

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

**First-use pack.** Committed, network-free, and not synthetic. Six queries an
agent put to Recall on 2026-07-27 on its first use of the tool, against the
configured home profile, plus the four things that session found wrong. Three
cases are enforced outright — a single-term query reaching a person's profile, a
clean abstention with complete coverage, and the excerpt case td-b94f6e closed,
which asserts that the text shown for a hit contains the term that produced it.
That last claim is made twice, on the two cases whose matching term sits in the
middle of its chunk rather than at the head, because it is the one defect here
that no ranking metric can see. Three cases remain marked `expected_fail`
against the tickets that fix them: a document that uses a word in an example
outranking the task that is one, one
record rendered as two results by two source instances over one catalog, and a
natural-language phrasing returning several times the results its keyword form
does. A marking comes off the case the moment it stops failing; the
`expected_failures_current` gate fails the run otherwise.

Its corpus is **pinned and committed**, which the development pack's is not, and
the reason is what it gates. Every change to ranking or admission has to pass
it, so it has to run in CI, and CI has no clara-home checkout, no Tasks store,
and no project-catalog instance. The pin is a git commit rather than a working
tree: documents are `git show 3c9b4c6:<path>` from clara-home, chosen because
that is the revision of `profile/TOOLS.md` holding the line the excerpt case is
about, which is gone from the file today. Tasks and project records are real
CLI and API output captured the same day and replayed. The corpus is trimmed to
what the six cases need and scrubbed of anything they do not; `eval/packs/
firstuse/sources/config.toml` records exactly what was kept, what was replaced,
and why. It is real prose nobody wrote to be retrieved, which is the one
structural defence against fixtures tuned to rank well, and it is small enough
that its ranking numbers are a regression baseline rather than a claim about
retrieval quality.

**Development pack.** Real questions from the configured system, over the two
sources of the first vertical slice: indexed project documents and live
structured tasks. It is what catches a change that improves fixture retrieval
while making real questions worse, which the smoke pack cannot see.
The paired `abstain-wifi-016` and `abstain-sentence-018` cases require a keyword
query and its natural-question form to abstain together; the documents query
analyzer made that phrasing invariant, so the pack requires 1.0 abstention
accuracy.

The development pack is private by contract. It lives outside every checkout,
is selected by an absolute path in the user layer, and contains 15–25 authored
cases spanning exact, paraphrase, implicit, cross-source, temporal, and
no-answer questions. Its judgments name evidence through the persisted locator
form `<source_uid>:<local locator>`; they never copy source bodies into the
pack. Any snapshot or replay material needed for determinism lives beside the
private pack and inherits the source corpus's sensitivity.

There is no built-in development-pack path. A machine opts in through its
uncommitted `$XDG_CONFIG_HOME/recall/config.toml`:

```toml
[evaluation]
development_pack = "/absolute/private/path/to/packs/dev"
development_baseline = "/absolute/private/path/to/baselines/dev.json"
```

Both paths must be absolute. A project `recall.toml` and `adapters.d` are
forbidden from declaring them. `recall eval validate` and `run` use
`development_pack` when `--pack` is omitted; `compare` uses
`development_baseline` when given only a candidate. An explicit path always
remains available for automation.

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
  schema/          pack, case, judgment, run schemas, plus embed.go
  packs/smoke/     pack.json, cases.jsonl, judgments.jsonl, sources/, transcripts/
  packs/firstuse/  pack.json, cases.jsonl, judgments.jsonl, sources/
  baselines/       smoke.json, firstuse.json
```

The repository holds schemas, two packs, and their baselines. It never holds
authored development questions, development judgments, personal absolute paths,
or run artifacts. Tests enforce the committed-pack allowlist and scan committed
evaluation artifacts for personal absolute paths.

The first-use pack is the one exception to "no copied source bodies", made
deliberately and bounded: it carries a trimmed, scrubbed snapshot of the home
corpus because the ranking work it gates runs in CI. Adding a third committed
pack means editing the allowlist test, which is the point of having one.

Run artifacts contain excerpts even when packs do not. They inherit the pack's
sensitivity, default to `$XDG_STATE_HOME/recall/<profile>/eval/`, and are never
committed.

A baseline is one such artifact's `run.json`, and only that file. The
`cases.jsonl` beside it carries excerpts and locators, so the baseline is the
record alone — which is why `recall eval compare` accepts a run directory or a
bare run record for either side. A comparison against a bare record reports
metric deltas and no per-case changes, because the baseline never claimed
anything about individual cases.

Refresh one deliberately, never as a side effect of a run:

```sh
recall eval run --pack eval/packs/smoke --output "$d"
cp "$d/run.json" eval/baselines/smoke.json

# Uses the private development_pack and development_baseline from user config.
recall eval run --output "$private_run"
cp "$private_run/run.json" "$private_development_baseline"
```

Changing any measured pack input changes its content hash, and a baseline
naming a different hash is not comparable at all. Tests assert the committed
smoke baseline names the committed smoke pack, so a pack edit that forgets the
refresh step fails in `make test` rather than surfacing in CI as a phantom
ranking regression. Private development baseline discipline is checked by the
local comparison.

Earlier commits contained a development-pack snapshot that crossed this
boundary. The current tree removes it, but ordinary deletion does not erase
immutable Git history. This change intentionally does not rewrite history; if
the repository was shared beyond its intended audience, treat the prior
snapshot as exposed and coordinate a separate history rewrite and cache review.

The hash covers the parsed manifest, cases, and judgments, plus the raw bytes
and slash-normalized relative path of every regular file in declared `sources/`
and `transcripts/` trees. Files are sorted before hashing. File mode, ownership,
mtime, and empty directories are ignored; they do not survive a portable copy
and do not affect an adapter. A missing declared tree is an error. Symlinks and
other non-regular entries are rejected rather than followed, including a
symlink in any intermediate component between the pack root and a nested
fixture root. Each file's full component path is revalidated immediately before
an opened-handle identity check and read. Fixture identity therefore cannot
escape the pack or depend on platform traversal behavior. An omitted tree
contributes no fixture entries; that absence remains represented by the
manifest field itself.

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
one named lineage must rank first
fewest and most results the response may carry
most result slots one record may occupy
substrings a named result's excerpt must contain
```

The last four came with the first-use pack, because they are what its six
queries are about and no ranking metric can state any of them. Graded metrics
score the shape of a whole list, so none of them says which single record is
the answer. A count is not a rank: the caller pays for every result, and a
phrasing that returns three times as many for no more information is a defect
that leaves nDCG untouched. Duplication across source instances is invisible to
judgments, which key on `source_uid` and so read two views of one catalog as
two pieces of evidence. And nothing else in the pack format reads what a result
*said*, which is the difference between a hit and evidence.

### Expected Failures

A case may declare `expected_fail`, which names assertions and says what has to
land before they can hold:

```json
{
  "expected_fail": {
    "reason": "td-92c7b7 for max_results, td-87eecf for max_results_per_record.",
    "assertions": ["max_results", "max_results_per_record"]
  }
}
```

Violations of a named assertion are recorded and do not fail the
`declared_assertions` gate. Everything else is enforced exactly as on any other
case — every assertion the case declares and does not name, plus its behavior,
coverage, provenance and locators.

It names assertions rather than excusing the case because a case-wide
exemption has a blast radius. Once a ranking failure is excused, a required
source that stopped being returned on the same case is excused with it, and a
real regression rides into a green build behind a defect somebody already knew
about. Naming them also keeps two tickets separable: the case above shows
movement when either fix lands, instead of reporting both as outstanding until
both are done and crediting neither.

The reason is required for the other half of the problem. An expected-failure
list with nothing to wait on is a mute button, and a marking with no defect
behind it has no way to ever come off. The `expected_failures_current` gate
closes that end: a **named assertion** that stopped failing fails the run, per
assertion and not per case, so the fix also forces the pack to record that the
movement happened. That is the whole reason for writing these cases before the
fixes rather than after.

A run records what each marking observed, in `expected_failures` on `run.json`.
It is there rather than only in the rendered summary because an excused
violation leaves `declared_assertions` reporting zero: a baseline without it
would freeze four known defects as nothing having happened, and `--json` would
disagree with what a person reads.

`recall eval validate` rejects a marking that names something outside the
assertion vocabulary, or an assertion the case does not declare. Both would
excuse nothing and then report themselves as fixed.

### Known Gaps

Writing the first pack found things a case cannot yet say. They are recorded
here rather than fixed by adding fields, because a field no case writes and no
metric reads is the dead-contract defect invariant 6 names. Each lands with the
pack that needs it:

- **An exclusion reason cannot be asserted.** `expected_source_outcomes` takes
  an outcome, so `unhealthy`, `denied`, `sensitivity_ceiling`,
  `as_of_unsupported`, and `budget_exhausted` all flatten to `skipped` —
  including the `as_of` case whose entire point is *which* reason was reported.
- **A match signal cannot be asserted.** The near-miss category exists to prove
  a substring does not exact-match, but `exact_identifier` is only observable
  through ranking, even though it is a deterministic partition and the
  explanation carries it.
- **An expansion's detail level cannot be stated**, so detail coverage lives in
  tags by convention.
- **A filesystem snapshot cannot express reproducible temporal
  behavior.** The documents adapter derives `as_of` from file mtime, which git
  does not preserve, so temporal cases belong on a stream source whose records
  carry their own event time. A development temporal case must use a
  deterministic boundary and ask the honest question: does the run report
  degraded coverage and abstain, or answer a past question from present state?

Writing the private development pack found four more contract edges. Two are
now enforced directly:

- **Every declared assertion is evaluated.** `required_sources` and
  `forbidden_sources` inspect the immutable source identities that contributed
  returned candidates; `visible_lineages` inspects final ranked roots; and
  `suppressed_lineages` requires positive suppression telemetry for that root,
  with reason `lineage_suppressed` and a non-zero candidate count. Mere absence
  from final ranking is not suppression. `max_latency_ms` measures case wall
  time. `max_expansion_bytes` is passed as the expansion budget and checked
  against returned content bytes; an answer case returning no expansion fails
  its precondition rather than passing a vacuous ceiling. `expected_revisions`
  likewise requires the named lineage to resolve. An `expected_behavior` of
  `abstain` is the schema's explicit allowance for a no-result case. A violation
  is named on the case score and fails the `declared_assertions` hard gate, so
  it cannot be diluted by a large pack.
- **The threshold vocabulary is closed.** Only
  `exact_identifier_success_at_1` and `abstention_accuracy` are accepted because
  those are the two absolute thresholds `EvaluateGates` implements.
  `ndcg_at_10` and `recall_at_5` remain ranking metrics protected by `compare`
  against a frozen baseline; placing either in `thresholds` is rejected rather
  than silently ignored.
- **Forbidden@5 may have a small population on a healthy corpus.** Forbidden
  evidence means superseded, out of scope, or over the ceiling. Reports always
  retain the metric's population count, so a passing value over one case is not
  mistaken for broad coverage.
- **Natural-language abstention exposes stopword behavior.** A development
  no-answer case must keep its authored wording even when a shorter reformulation
  would pass. Quietly editing a real query until it abstains would delete the
  finding the pack exists to preserve.

## Reproducible Execution

Every deterministic run records: Recall commit and dirty-tree state; pack
identity and content hash; resolved profile hash with secrets removed; adapter
and index versions; model names and artifact hashes; host OS, architecture, and
memory; cache policy and warm/cold state; random seeds; and per-case source
outcomes, candidates, lineage roots, explanations, and timing.

Each case's `as_of` bounds the request, so sources answer the historical
question instead of treating the boundary as future-dated and replying from
current state. Network access is disabled. Model-backed and network-backed adapters replay recorded protocol
transcripts (see [conformance](adapter-protocol.md#conformance)) rather than
running live.

```text
recall eval validate [--pack <path>]
recall eval run      [--pack <path>] --output <dir>
recall eval compare  [<baseline>] <candidate>
recall eval report   <run>
recall eval export-trec <run>          (stage 2)
```

A run produces `run.json` (environment, metrics, gates, status, and what each
declared expected failure observed), `cases.jsonl` (one detailed result per
case), `summary.md`, and `results.trec` when applicable.

`run` exits non-zero when a hard gate fails. `compare` exits non-zero when the
two runs are not comparable **or** when any rate moved down, overall or in any
case-tag group. Both are the same verdict to a script: do not promote this.
Latency is not compared — it is a property of the machine, and a baseline frozen
on one host would fail every build on another. CI runs both committed packs
against their committed baselines, which is what `make eval` does:

```sh
for p in smoke firstuse; do
  recall eval run     --pack "eval/packs/$p" --output "$d/$p"
  recall eval compare "eval/baselines/$p.json" "$d/$p"
done
```

A change to ranking or admission has to pass that target. The smoke pack covers
the failure vocabulary a healthy corpus cannot produce; the first-use pack
covers six real queries whose answers a person checked, which is the only thing
that catches a change improving synthetic retrieval while making a real
question worse.

The private development pack remains a required local development and release
gate on top of both. It is larger, its questions are authored rather than
observed, and CI cannot access it and must never substitute a committed copy.

A rate is called a regression when it moves down by more than 1e-9. That is not
slack for real movement: the smallest change one case can make to a forty-case
average is around 1e-3. It exists so a difference in the last bit of a float
between two architectures is not reported as a loss in retrieval quality.

Comparison applies that rule to every defined overall rate, every case-tag
group, and every source-family group and its macro. Any one loss makes the
comparison unacceptable and `recall eval compare` exits 3; an overall gain
cannot hide a family-specific loss. A source-family metric that had a positive
baseline population and becomes undefined (`N=0`) is also a loss: allowing a
source to improve its verdict by disappearing would be worse than ignoring a
numeric decline. The reverse transition remains undefined rather than being
claimed as an improvement, because it has no baseline.

Machine-readable deltas carry both population counts, explicit `dimension`,
`population`, and `group` fields, plus stable qualified keys such as
`tag:group:documents`, `source_family:group:documents`, and
`source_family:macro`. Thus a tag and a source family with the same display name
remain different populations. Human regression labels spell out the dimension,
group or macro, and both `N` values for the same reason.

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
latency       wall time around one query, never the difference between
              injected clocks. A case bounded to a past instant is asking a
              historical question, not taking months to answer it.
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

**Always say which population a number came from.** Overall and macro are
different measurements of the same run and they do not agree by construction:
overall pools the cases where a metric is defined, the case-tag macro weights
each tag group equally, and the source-family macro weights each family equally.
Quoting one and later reading another off the bottom of `summary.md` can invent
a regression that never happened. A metric written down without its population
is a trap laid for the next reader. Smoke, first-use, and private-development
numbers are three different populations over three different corpora and must
never be compared to one another.

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
- Every declared case assertion holds, except the ones an `expected_fail`
  marking names.
- Every assertion an `expected_fail` marking names is still failing.
- The run completes with no fixture mutation, crash, or undeclared network use.
- No project-configuration fixture caused a subprocess spawn.

Thresholds are absolute values recorded in the pack. They stay absolute: they
are floors on behavior a release must clear at all, and they are checked by
`run` on a single run with nothing to compare against.

Regression protection is the separate job of `compare` against the pack's
baseline. The smoke and first-use baselines are committed; the development
baseline is private. On every pack comparison is exact rather than statistical.
Every rate is deterministic — the same pack over the same commit produces the
same number to the last bit, on repeated runs and across ten commits — so run
variance is zero and any downward movement is a real change to explain. A pack
whose numbers do move between identical runs would need distributions and an
interval instead; this one does not, and pretending otherwise would buy slack
that only hides regressions.

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

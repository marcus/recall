# Recall Evaluation Framework

**Status:** Proposed

**Related decision:** [ADR-0002](adr/0002-evaluation-framework.md)

Recall needs an evaluation loop that answers a narrow question: did this change
retrieve better evidence without making exact lookup, abstention, provenance,
privacy, source honesty, latency, or cost worse?

The framework has two layers:

1. Deterministic retrieval evaluation is the development and release gate.
2. Answer-utility evaluation checks whether a fixed agent uses the retrieved
   evidence well. It is slower and may use model judges.

An aggregate retrieval score is useful for experiments. It never overrides a
failed safety or correctness gate.

## Staging

The framework ships in two stages so evaluation infrastructure cannot
front-run the product:

**Stage 1 — with the first vertical slice.** Case, judgment, and run schemas;
the smoke pack; `recall eval validate`, `run`, `compare`, and `report`; the
retrieval metrics; and the hard acceptance gates. This is everything needed for
a trustworthy baseline and regression protection.

**Stage 2 — after a trusted baseline exists.** The research score, the private
holdout and its governance, TREC export and `trec_eval` cross-checks, answer-
utility evaluation, and the autonomous research loop. Sections below that
belong to stage 2 are design commitments, not stage-1 work.

## Evaluation Packs

An evaluation pack is a versioned set of queries, source fixtures or snapshot
references, relevance judgments, policy assertions, and budgets.

Recall should support these pack types:

### Smoke Pack

A small, synthetic pack committed to the repository. It runs on every change
without network access and covers:

- Exact identifiers and aliases
- Lexical and semantic paraphrases
- Cross-source fusion and duplicate lineage
- Current, historical, stale, and superseded evidence
- Answer, clarify, and abstain behavior
- Unavailable, denied, partial, and timed-out sources
- Expansion and locator revision checks
- Suppression behavior

### Private Development Pack

Real questions drawn from the configured personal or work system. It stores
queries and judgments in a private pack outside the public source tree and
resolves evidence by stable locator. Source bodies do not need to be copied into
the pack.

This pack is visible to a human researcher and may be visible to an automated
research agent. It is used for iteration.

### Private Holdout Pack

Real questions withheld from implementation and research agents. A trusted
runner evaluates a proposed improvement after development results are frozen.
The holdout should differ by topic, time range, and source mix rather than being
a random sample of near-duplicate questions.

### Live Health Pack

A small read-only pack that checks configured live sources. Its results measure
operational drift and are not comparable enough to serve as the optimization
score. Live records change; fixtures and snapshots do not.

## Pack Layout

The planned repository layout is:

```text
eval/
  schema/
    pack.schema.json
    case.schema.json
    judgment.schema.json
    run.schema.json
  packs/
    smoke/
      pack.json
      cases.jsonl
      judgments.jsonl
      sources/
  baselines/
    smoke.json
  research/
    program.md
```

Private packs can live elsewhere and be selected by absolute path or profile.
The system repository contains schemas, synthetic fixtures, and public
baselines. It does not need personal or employer source data.

## Case And Judgment Model

Queries and judgments are separate files. This lets a runner withhold judgments
from an agent while still giving it the development queries.

A case contains:

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
  "notes": "Exact stable identifier should route to work items."
}
```

A judgment targets an evidence lineage rather than rendered text:

```json
{
  "schema_version": 1,
  "case_id": "tasks-exact-001",
  "lineage_id": "tasks:td-f62256",
  "relevance": 3,
  "required": true,
  "forbidden": false,
  "supports": ["state", "title"]
}
```

Relevance uses a graded scale:

```text
0  irrelevant
1  related context
2  useful supporting evidence
3  direct or authoritative evidence
```

A case may also declare policy assertions that are not relevance judgments:

```text
expected source outcome
required or forbidden source
required abstention or clarification
maximum latency
maximum expansion size
locator must resolve
candidate must be suppressed or visible
no candidate may cross a permission boundary
```

## Reproducible Execution

`recall eval` runs through the same application layer as the CLI and MCP
surface. There is no evaluation-only ranking implementation.

Every deterministic run records:

- Recall commit and dirty-tree state
- Pack identity and content hash
- Resolved profile hash with secrets removed
- Adapter and index versions
- Model names and artifact hashes when models are used
- Host, operating system, architecture, and available memory
- Cache policy and whether the run was warm or cold
- Random seeds
- Per-case source outcomes, candidates, lineages, score explanations, and timing

Fixture runs inject the clock from each case's `as_of` value. External network
access is disabled. Model-backed retrieval uses pinned local artifacts or a
recorded deterministic response fixture in the smoke pack.

The initial command contract should be:

```text
recall eval validate --pack <path>
recall eval run --pack <path> --output <dir>
recall eval compare <baseline> <candidate>
recall eval report <run>
recall eval export-trec <run>
```

A run produces:

```text
run.json               environment, aggregate metrics, gates, and status
cases.jsonl            one detailed result per case
results.trec           standard ranked output when applicable
summary.md             concise human-readable comparison
```

## Retrieval Metrics

Recall should implement a small set of standard information-retrieval metrics
in Go and verify them against published test vectors and NIST `trec_eval`.
The runner also exports standard TREC run and qrels files.

The core metrics are:

| Metric | Use |
| --- | --- |
| nDCG@10 | Primary graded ranking quality near the top |
| Recall@5 and Recall@20 | Whether required evidence was retrieved |
| MRR@10 | Rank of the first direct answer, especially exact queries |
| Success@5 | Fraction of cases with useful evidence near the top |
| Forbidden@5 | Rate of forbidden or superseded evidence near the top |
| Abstention accuracy | Correct answer, clarify, or abstain behavior |
| Locator success | Returned evidence references that expand correctly |
| Provenance accuracy | Candidates with correct source and lineage |
| Source-outcome accuracy | Failures, denial, and partial coverage reported honestly |
| p50 and p95 latency | Typical and tail response time |
| Query cost | Model calls, tokens, and external requests |

Metrics are reported by case tag, source family, and the macro average across
groups. One large document source must not hide regressions in exact tasks,
temporal questions, or no-answer cases.

## Acceptance Gates

A candidate run is invalid when any hard gate fails. Initial gates should
include:

- Zero permission leaks
- Zero forbidden results for security-critical cases
- All returned locators resolve to the judged lineage and revision
- Source failure and partial-coverage fixtures report the expected outcome
- Exact-identifier Success@1 does not regress
- Abstention accuracy does not regress
- p95 latency and query cost stay within the pack budget
- The run completes without fixture mutation, crash, or undeclared network use

Thresholds become pack configuration after the first baseline. They should not
be copied from Scent or another retrieval system.

## Research Score

Runs that pass every gate receive a development research score:

```text
research_score =
    0.50 * macro_nDCG@10
  + 0.25 * macro_Recall@5
  + 0.15 * macro_MRR@10
  + 0.10 * macro_Success@5
```

These weights are an initial proposal. Baseline analysis may change them before
the first autonomous experiment.

The score exists to order experiments. Release decisions also require:

- No meaningful regression in any case group
- A minimum improvement larger than normal run variance
- A paired per-case comparison with a confidence interval
- Review of newly failing or newly passing cases
- A complexity check covering code size, dependencies, and operational cost

When quality is equal, simpler code wins.

## Answer-Utility Evaluation

Retrieval metrics cannot prove that an agent uses evidence correctly. A separate
suite sends the same query and Recall output to a fixed answer model and prompt.
It measures:

- Supported-claim precision and recall
- Citation or locator correctness
- Unsupported-claim rate
- Answer completeness
- Correct abstention and clarification
- Prompt-token, latency, and model cost

The model, prompt, temperature, provider, and judge rubric are versioned. A
model judge never creates ground truth silently. Human-reviewed examples anchor
the rubric, and disagreements or low-confidence judgments remain visible.

Answer evaluation runs less often than retrieval evaluation. The first
autoresearch loop should optimize deterministic retrieval only.

## Karpathy-Style Autoresearch Loop

Karpathy's `autoresearch` works because the editable surface is small, the
evaluation code is fixed, each experiment has the same time budget, one metric
orders results, and every attempt is kept or discarded in a simple log.

Recall should adopt that discipline with stricter gates.

### Immutable During A Research Run

- Evaluation runner and metric implementations
- Pack schemas, fixtures, cases, and judgments
- Private holdout pack
- Dependency lockfile
- Experiment time, cost, cache, and hardware policy
- Permission and provenance gates

The runner verifies hashes before and after every experiment.

### Editable Surface

Each research campaign declares its allowed paths. A first campaign might allow:

```text
internal/ranking/
internal/routing/
research/config/
```

Adapters, public contracts, evaluation code, and test data stay out of scope
unless the human defines a different campaign.

### Experiment Record

Every attempt appends one record to `experiments.jsonl`:

```json
{
  "experiment_id": "20260723-014",
  "parent_commit": "abc1234",
  "candidate_commit": "def5678",
  "hypothesis": "Lower the RRF constant for exact identifier routes.",
  "changed_paths": ["internal/ranking/rrf.go"],
  "status": "keep",
  "research_score_before": 0.7124,
  "research_score_after": 0.7191,
  "gates": "pass",
  "p95_ms_before": 41.2,
  "p95_ms_after": 40.9,
  "description": "Improved exact and task groups without regressions."
}
```

The full run artifacts live outside Git or in ignored storage. The experiment
log and accepted code changes are reviewable.

### Loop

1. Create an isolated `autoresearch/<tag>` branch or worktree.
2. Validate pack and environment hashes.
3. Run and record the unchanged baseline.
4. Form one falsifiable hypothesis.
5. Change only allowed files and commit the candidate.
6. Run the fixed development pack inside the experiment budget.
7. Discard crashes, gate failures, and non-improvements.
8. Keep a passing improvement and use it as the next baseline.
9. Stop at the campaign experiment, time, or cost budget.
10. Freeze the best candidate and ask a trusted runner to evaluate the holdout.

The loop cannot merge to `main`, alter the holdout, increase its own budget, add
dependencies, enable network access, or publish results. A human reviews the
candidate, holdout report, and complexity before promotion.

## Avoiding Benchmark Overfitting

- Split by topic, time, source family, and query form.
- Keep paraphrases and near-duplicates in the same split.
- Maintain a private holdout that research agents cannot read.
- Add production failures as new regression cases, then rotate related holdout
  cases.
- Inspect per-group changes instead of optimizing only the aggregate.
- Limit experiments per campaign and record every attempted hypothesis.
- Re-run finalists from a clean checkout.
- Require gains to survive at least one alternate pack or time slice.

An agent may discover a better ranking policy. It may not encode case IDs,
expected locators, query strings, or source-specific answers into production
logic.

## External Compatibility

Recall should borrow formats and validation, not a Python runtime:

- TREC qrels and run files provide a standard interchange for graded judgments
  and ranked results.
- NIST `trec_eval` can cross-check nDCG, recall, success, and related metrics.
- BEIR provides useful public heterogeneous retrieval datasets and baselines,
  but its Python framework is optional.
- Generic LLM evaluation tools may drive answer-utility experiments by calling
  Recall's CLI or API. They do not replace Recall's source-health, lineage,
  temporal, permission, and expansion assertions.

## First Milestone

Before ranking implementation begins:

1. Finalize the case, judgment, and run schemas.
2. Write 30 to 50 synthetic smoke cases.
3. Assemble 50 to 100 real development cases across the first three sources.
4. Reserve at least 25 real cases as a hidden holdout.
5. Implement the lexical and unweighted-RRF baseline.
6. Record its metric distribution and latency on the target machine.
7. Set gates and a minimum meaningful improvement from that evidence.

The first goal is a trustworthy baseline, not an overnight research loop.

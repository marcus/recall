# ADR-0002: Evaluation Framework

## Status

Proposed

## Context

Recall needs a stable answer to "is retrieval getting better?" It must compare
exact, lexical, semantic, structured, temporal, and fused retrieval across
heterogeneous sources. It must also catch regressions in abstention, permission
handling, provenance, source health, expansion, latency, and cost.

The evaluation framework should support bounded autonomous experiments without
letting an agent modify its own grader or overfit to a visible holdout.

## Decision Drivers

- Deterministic local runs for core retrieval
- Graded evidence judgments and standard rank metrics
- First-class Recall locators, lineage, permissions, and source outcomes
- Public synthetic fixtures plus private real-world packs
- No required Python runtime
- Machine-readable comparisons and human-readable failure reports
- A fixed evaluator suitable for autonomous keep-or-discard loops

## Considered Options

### Native Recall Harness With TREC Compatibility

Implement pack validation, execution, Recall-specific gates, comparisons, and a
small metric set in Go. Export standard TREC qrels and run files and cross-check
metric implementations against NIST
[`trec_eval`](https://github.com/usnistgov/trec_eval).

This handles Recall's non-ranking contracts without making the core dependent
on another language. Standard interchange keeps the results inspectable.

### BEIR Or A Python IR Framework

[BEIR](https://github.com/beir-cellar/beir) supports heterogeneous retrieval
datasets, lexical and neural retrievers, and nDCG, MAP, recall, precision, and
MRR. It is useful for external baselines and public model comparisons.

It does not model Recall's adapter health, permissions, lineage, temporal
validity, evidence expansion, or suppression. Adopting it as the primary runner
would also reintroduce Python into the core workflow.

### Generic LLM Evaluation Framework

Generic evaluation tools are useful for sending Recall evidence through a fixed
answer model and applying claim-support rubrics. Model-judged scores are slower,
provider-dependent, and less reproducible than evidence judgments. They cannot
be the core development gate.

### Hosted Evaluation Service

A hosted service would improve dashboards and collaboration but would send
queries, judgments, or evidence outside the local system. It adds cost and
dependency risk without solving Recall's custom correctness rules.

## Proposed Decision

Build `recall eval` as a native module in the Recall core.

Use versioned JSON/JSONL evaluation packs with separate cases and graded
lineage judgments. Implement nDCG@10, Recall@5/20, MRR@10, and Success@5 in Go.
Add Recall-specific gates for forbidden evidence, abstention, locator
resolution, provenance, source outcomes, permissions, latency, and cost.

Export TREC qrels and run formats. Use NIST `trec_eval` as an optional
cross-check in development and CI, not as a runtime dependency.

Maintain a committed synthetic smoke pack, an external private development
pack, a hidden private holdout, and a non-comparable live health pack.

Add answer-utility evaluation later as a versioned outer layer. Retrieval
evaluation remains the first release gate.

## Autonomous Research Policy

Borrow the fixed-harness pattern from Karpathy's
[`autoresearch`](https://github.com/karpathy/autoresearch):

- Fixed evaluator, data, dependencies, time budget, and resource policy
- Narrow declared edit surface
- Baseline first
- One committed hypothesis per experiment
- Machine-readable keep, discard, or crash log
- Simpler code wins when quality is equal

Recall adds hard safety and correctness gates before a research score is
considered. The agent cannot read or modify the holdout, change budgets, add
dependencies, enable network access, merge to `main`, or publish results.

## Consequences

Recall owns a modest amount of metric and fixture code. Standard formulas and
`trec_eval` cross-checks keep that code honest.

The framework can evaluate failures that ordinary IR tools cannot represent.
It also keeps private source data local and separates deterministic retrieval
progress from model-judge variance.

A single research score makes autonomous iteration possible, but it may hide
group regressions. Hard gates, macro reporting, per-case diffs, held-out
evaluation, and human review remain mandatory.

## References

- [Recall evaluation framework](../evaluation.md)
- [NIST trec_eval](https://github.com/usnistgov/trec_eval)
- [BEIR](https://github.com/beir-cellar/beir)
- [Karpathy autoresearch](https://github.com/karpathy/autoresearch)
- [Karpathy autoresearch agent program](https://github.com/karpathy/autoresearch/blob/master/program.md)

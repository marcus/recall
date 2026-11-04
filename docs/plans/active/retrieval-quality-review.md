---
type: plan
title: Recall accuracy, speed, and agent experience review
description: Architecture assessment and proposed improvement sequence for discussion.
status: proposed
generated: true
verified: false
tags: [recall, architecture, retrieval, evaluation]
---

# Recall improvement plan

Reviewed 2026-11-04 at `a4277950b5f8ede8f7346cee6a453814ccf4c591`. This is a proposal, not authorization to implement it. Scope: source inspection of orchestration, lexical retrieval, fusion, expansion, evaluation and QMD contracts, plus focused tests. No production performance benchmark, work-corpus evaluation, or live QMD model comparison was performed. The installed CLI reports `v0.6.0-1-g83dc137`; distinguish it from the reviewed checkout in all future measurements.

## Assessment

Keep the architecture. Recall has a useful modular core: transports call the application layer; source adapters own retrieval; the planner resolves eligibility and budgets; ranking groups lineage and fuses candidates; evidence shaping controls output size. Coverage failures remain distinct from abstention. Sensitivity checks precede ranking. Derived copies are not independent corroboration. These are valuable foundations for trustworthy agent retrieval.

It is not yet defensible to call Recall state of the art on the user's workload. The missing evidence is comparative performance on representative work and home questions, especially paraphrases and multi-source questions. Algorithm availability is not proof of effective integration.

Several old complaints are already addressed: `internal/adapters/docs/tokenize.go` uses query-time number variants, JSON has a compact pointer tier, planning runs concurrently, and repeated post-search health probes have been removed. Do not schedule these again.

## Findings and priorities

### 1. Accuracy: semantic and lexical relevance are not interchangeable

`internal/ranking/group.go:250` defaults missing relevance to 1; group scoring multiplies relevance by the source prior and reciprocal rank. This preserves legacy behavior but gives unknown relevance the strongest factor. `cmd/recall-qmd/qmd/doc.go` explicitly documents that vector cosine and lexical relevance are not comparable, that vector results can be systematically discounted in mixed profiles, and that hybrid/full modes have admission limitations. Consequently, adding a strong embedding model alone does not guarantee stronger final retrieval.

First measure retrieval at three points: adapter candidate pool, admitted pool, final top results. Separate failure to retrieve, rejection by admission, and poor final ordering. Make relevance basis and missing relevance explicit in the ranking contract. Compare basis-specific calibrated admission followed by rank fusion against the existing multiplicative approach. Calibration must use development judgments, with a separate frozen holdout. Do not interpret a fused score or cosine as a universal answer-confidence probability. Preserve exact-ID behavior, lineage suppression, and coverage honesty.

Acceptance: fewer paraphrase and cross-source misses on held-out questions; no forbidden evidence, exact-ID regressions, or additional false answers on no-answer cases. A migration rule for legacy adapters must be explicit and tested rather than silently changing their scores.

### 2. Accuracy: evaluate the actual work/home journey

The repository has 50 smoke, 6 shapes, and 10 QMD-mode cases. These are valuable regression fixtures. `docs/evaluation.md` specifies a private development pack of 15–25 cases and describes private holdout and answer utility as stage-two work. This review did not inspect the private development pack or establish that a holdout exists elsewhere.

Extend the existing evaluator rather than build another. Start with roughly 100–200 judged questions split between work and home, kept in their respective environments. Include exact IDs, acronyms, natural questions, paraphrases with no lexical overlap, multi-source evidence, current versus superseded decisions, stale/deleted documents, legitimate no-answer questions, and unavailable sources. Pool judgments from competing methods so new methods are not punished merely for finding previously unjudged evidence. Keep development and holdout topics separated and preserve corpus/config/model revisions.

Measure Recall@candidate-limit, nDCG@10, MRR, false-answer/false-abstention rates, required-evidence coverage, and query-to-successful-expansion utility. Report by query family and environment. Use paired comparisons and uncertainty; a tiny aggregate gain should not justify added machinery. Never copy work source material to the personal repository or public services as part of evaluation.

### 3. Agent experience: expansion needs a stronger changed-evidence contract

`internal/adapters/docs/expand.go:118` detects changed file content and reports a new revision, but still reads the original line range. Insertions above a saved locator can therefore produce a different passage. This is transparent revision reporting, but not stable passage resolution. `internal/app/expand.go` deliberately treats expansion as independent of the original query.

Add an optional expected revision/fingerprint to expansion across CLI/API/MCP, with a typed changed-evidence outcome. Offer an explicit latest-content mode. Where a chunk fingerprint uniquely identifies the original passage, relocate and report the new range; otherwise require a fresh query rather than guessing. Keep old locators usable under documented latest semantics. Test edits before the section, section rewrites, duplicates, deletion, and a refreshed index between query and expansion.

Acceptance: an agent can distinguish the evidence it selected from changed content without parsing text or manually comparing opaque revision strings.

### 4. Speed: measure startup and planning before changing storage

`internal/source/plan.go:284` already runs source consideration concurrently; `internal/app/query.go` separately fans out searches. Both phases wait for their phase's sources. `internal/adapters/docs/index.go:541` decodes the generation and derives search structures when opening it. Cold CLI startup can therefore differ substantially from repeated requests to a warm service. These are traced cost centers, not measured bottlenecks.

Instrument wall time, CPU, allocation/RSS, process launches, and time spent in config, adapter initialization, health/preparation, retrieval, fusion, shaping, and expansion. Benchmark exact checkout binaries against fixed corpora with small and realistic source counts. Separate cold CLI, warm service, local-only, and mixed/remote profiles. Include cancellation and several concurrent callers. Report p50/p95/p99 and result equivalence, not just average query time.

Optimize whichever dominates: reuse initialized adapters and generations in the existing service; avoid redundant subprocess work; consider overlapping per-source preparation and search if the global planning barrier dominates, while preserving duplicate-store refusal before fusion. Introduce concurrency limits only against measured resource pressure. Adopt a persisted compact index only if decoding demonstrably dominates at real corpus size. Do not change storage simply because SQLite or a vector database is fashionable.

Acceptance: a material reduction in measured end-to-end p95 with equivalent evidence and honest coverage. Set numerical SLOs after the baseline. No hidden source omission to make latency look better.

### 5. Retrieval experiments: use existing seams

QMD already provides lexical, vector, hybrid, and reranked paths. Establish lexical-only, vector-only, and combined baselines through the current adapter before adding another engine. Then compare a bounded reranker, contextualized chunks, and optional multi-query retrieval separately. Record additional latency, model memory, cost, provider disclosure, and admission behavior. Every experiment retains a deterministic lexical fallback with explicit degraded semantics where appropriate.

A reranker cannot recover evidence missing from its input pool. Heading and document context may improve chunk retrieval, but generated contextual summaries can introduce errors; retain originals and provenance. Query rewriting must preserve identifiers and filters. Broad graph retrieval or autonomous research loops are deferred until the held-out set demonstrates a need for relationship traversal or repeated search.

## Delivery sequence

1. Baseline and fixtures: verify deployed versions, assemble environment-local judgments, instrument stages, and freeze current results. Deliver a reproducible quality/performance report.
2. Correctness and contract slice: address relevance-basis calibration and changed-evidence expansion, each behind compatible behavior and its own evaluation. Deliver before/after cases and regression results.
3. Performance slice: fix the measured dominant startup/planning/retrieval cost. Deliver latency distributions, resource measurements, and evidence-equivalence checks.
4. Retrieval tournament: run existing QMD modes plus bounded reranking/context experiments. Promote only improvements that survive held-out evaluation and operational budgets.
5. Agent utility: measure complete find/select/expand tasks with fixed harness/model settings, then spot-check a second harness. Add machine-readable next actions only where actual failures show agents need them. Keep the core harness-agnostic.

For discussion: choose the initial work query families, acceptable optional model latency, and whether semantic processing must remain local at work. These determine experiment scope; they do not prevent the baseline and deterministic contract work.

## Verification and research basis

Passed: `go test ./internal/app ./internal/ranking ./internal/adapters/docs ./internal/eval ./internal/source`. This is focused regression evidence, not a security audit, full-suite verdict, live integration acceptance, or production speed claim. No source code, installed binaries, configuration, or production indexes were changed.

Research supports testing hybrid retrieval, reranking, context, and agent task utility; it does not establish which configuration wins here:

- [BEIR](https://arxiv.org/abs/2104.08663): heterogeneous evaluation and strong lexical baselines.
- [Anthropic contextual retrieval](https://www.anthropic.com/research/contextual-retrieval): contextual lexical/embedding retrieval and reranking experiments; its reported gains are not Recall forecasts.
- [BRIGHT](https://arxiv.org/abs/2407.12883): reasoning-intensive retrieval as a distinct challenge.
- [BRIGHT-Pro](https://arxiv.org/abs/2605.04018): multi-aspect evidence and static versus agentic search evaluation.


## Initial evaluation slice

The authorized evaluation work added a pack census to `eval validate` and an environment-local baseline guide in commit `e88a6c8`. Parent review reran the evaluator/CLI tests and exercised JSON validation on the synthetic shapes pack. Private home artifacts remain outside the repository.

A first frozen home development seed contains 15 project document snapshots and 34 agent-authored cases. The parent verified snapshot hashes and identical rankings across two runs; only latency differed in per-case scores. The run fails its abstention gate and is diagnostic evidence, not a passing release baseline. Its convenience sampling, artificial no-answer stress questions, section-specific judgments, and single lexical adapter prevent claims about everyday home/work accuracy or semantic/lexical calibration. No work corpus or independent holdout has been evaluated.

Next evaluation work should review alternate valid evidence, collect realistic unanswered questions, and add mixed-source cases in each environment. Keep this seed frozen as a diagnostic population, separate from those new populations.

For relevance comparability, recommend an experimental separation between basis-specific admission and rank-based fusion after admission. Missing relevance should be explicit unknown evidence, with a versioned compatibility policy. Compare candidate retrieval, admission losses, and final ordering against current behavior; preserve exact lookup, lineage and coverage guarantees. Do not fit new thresholds from the single-adapter seed or change production defaults until independently judged mixed-source development and holdout results support it.

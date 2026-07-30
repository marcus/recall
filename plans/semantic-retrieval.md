# Semantic Retrieval For The Document Corpus

Status: decision pending. Written 2026-07-29.

Question: would Recall's document adapter be significantly improved by adding
semantic retrieval, and if so, at what layer?

Short answer: yes, at one layer — but the recommended first step is not to build
anything. Configure QMD as an additional source over the same corpus, measure it
against the lexical baseline, and let that result decide whether a reranker is
worth building.

## The Gap That Is Real

`internal/adapters/docs` is lexical only, by declaration
(`internal/adapters/docs/doc.go`). Its honest failure mode is vocabulary
mismatch. `minimumQueryTerms = 2` in `internal/adapters/docs/search.go` requires
a candidate to carry two of a question's content terms, so a paraphrase sharing
one word with the answer returns nothing — the documented "what is the zxqv
project" abstention. The number-variant machinery (`recall.ResolveTermVariants`,
`recall.NumberVariantWeight`) is a hand-rolled morphology patch on the same
crack: a symptom of lexical matching being asked to do a semantic job.

Dense or hybrid retrieval would measurably improve recall on paraphrased
questions. That gap is not hypothetical.

## The Gap That Is A Different Bug

The known dentist defect is **not** a retrieval miss. Both records are already
in the pool. It is `recall.Relevance` (`pkg/recall/relevance.go:33`):

```
coverage × concentration,  concentration = density/(density + ref),  density = hits/length
```

This is a proxy for aboutness assembled from term density. It cannot distinguish
a four-term task title that *is* a dentist errand from a four-hundred-term chunk
that merely mentions one, except by accident of length. Three separate patches
prop up the same proxy: `MaxPerSource`, `RelevanceFloor`, and the quoted-
occurrence discount (`internal/adapters/docs/quotes_test.go`).
`internal/ranking/config.go:88` records the trap directly — 0.30 is both the
share that fixes result volume and the share that breaks dentist-001. No setting
of a density heuristic satisfies both, because density is not the quantity
anyone cares about.

**Consequence for this decision: a source that returns its own scores does not
fix the dentist defect.** Only a reranker, or a different relevance definition,
addresses it.

## What Must Not Break

Recall's differentiator is not retrieval quality; it is honesty — abstention
(exit 2), degraded coverage (exit 3), and withheld-with-reason accounting.

Dense retrieval has no native "nothing matched." Cosine similarity always
returns a ranked list with plausible scores. Every admission floor in the docs
adapter is expressible as *"the chunk carries N of the terms you asked for."*
There is no dense analogue that survives being written down as a rule.

**The hard problem is not embedding. It is defining an admission floor in vector
space that does not turn `outcome abstained` into `outcome answered` with
confident garbage.** Any option below that cannot answer this should not ship as
the default document source.

## Option A — Cross-Encoder Reranker (build)

### What it is

A bi-encoder embeds query and chunk independently, then compares by cosine;
independence is what makes it indexable and also what makes it blunt. A
cross-encoder takes `(query, chunk)` as a single joint input and emits one
scalar, so attention runs across both sides: it can see negation, passing
mention versus subject, and citation versus assertion.

It therefore cannot retrieve (O(corpus) per query, no index) and only ever
reranks an existing candidate pool. That is why it is cheap conceptually: no new
index generation, no per-generation embedding model id, no corpus rebuild, no
whole-corpus egress. It is a pure function over candidates already in memory.

### Why it lands on the dentist defect

It answers the actual question — does this passage answer this query — directly,
and is indifferent to length and to quoting. The three patches above become
unnecessary rather than retuned.

### Placement

`docs/spec.md:1237` says local ML runs behind adapters, imagining *source*
adapters. That placement does not work here: per-adapter reranking means every
adapter carries the model (including `td` and `tasks`, which are thin CLI
shells), N model loads, and per-source scores comparable only by convention. The
dentist case is cross-source — a tasks record losing to a docs chunk.

Recommended: a **rerank adapter that is not a source adapter** — separate
process, same trust and egress boundary, one model, invoked once by fusion on
the fused top-k. Requires amending `docs/spec.md:1237` to name a second adapter
role, explicitly rather than by smuggling.

### The contract decision that comes first

What does the reranker's score mean to the pipeline?

1. **Overwrite `Relevance`.** Tempting — `RelevanceFloor` finally becomes a
   calibrated cutoff. But `Relevance` is *declared by the source* (docs
   discounts its own citations; that declaration is load-bearing and
   per-corpus), and it is the number every graded judgment was tuned against.
   Makes the reranker load-bearing for abstention.
2. **New field, ordering only.** Feeds `compareRelevance`
   (`internal/ranking/select.go:16`) within the exactness partition; admission,
   floor, withholding, and abstention stay lexical. **Recommended.** Reranking
   is an ordering improvement, so a reranker that is slow, missing, or wrong
   degrades result *order* and never the honesty contract.
3. **Feed the fused score.** No. `internal/app` already forbids thresholding
   that by name, as a number pretending to be a confidence.

### Cost

`go.mod` is currently three dependencies, pure Go, no cgo, one static binary —
the shape argument in `docs/spec.md:1234`. Ways out:

| Approach | Cost |
|---|---|
| ONNX Runtime via cgo | Native lib per platform, cgo in build, harder cross-compile and release |
| Sidecar process (Python/Rust) | Go binary stays clean; adds a runtime the user must install |
| llama.cpp / candle subprocess | Lighter runtime, weaker reranker model selection |
| Pure-Go inference | Not realistically available at usable speed |

Sidecar is right, and the protocol cost is near zero: `cmd/recall-stream` is a
working, conformance-tested out-of-process adapter to copy.

New regardless of approach:

- **Model weights as a distributed artifact** — versioned, integrity-checked,
  tokenizer matched to model. `recall init` must acquire or locate it, and
  handle absence and mismatch. New first-run failure class, in a project that
  just hardened first-run (108f509).
- **Determinism.** `internal/eval/hash.go` hashes packs to a content identity,
  the README publishes byte-exact CLI transcripts, and `search.go` makes
  tie-breaks total so rebuilds order identically. Neural inference is not
  reproducible in that sense (float accumulation varies with threads, batching,
  hardware, library version). Mitigation, decided up front: quantize the
  reranker score to fixed decimals so near-ties fall into the existing
  deterministic chain (score → path → chunk ordinal), and treat model id+version
  as part of the eval run's identity, so a model change is a new baseline rather
  than a regression. Skipping this yields intermittent fixture failures — the
  failure mode most likely to get the feature reverted.
- **Latency.** ~50 forward passes on the hot path of every query. Must be
  cancellable and must participate in existing coverage reporting: a rerank that
  timed out is a *degraded* answer, not a silently lexical one.
- **Two permanent ranking paths.** Reranker on and off must both stay correct,
  because off is the fallback. Every future ranking change is evaluated twice.
- **Input window.** Cross-encoders take a few hundred tokens; heading-scoped
  chunks exceed that. The scored span must be the same span the excerpt logic
  anchors on, or the shown excerpt will not correspond to why the result ranked.
- **Local only**, per `docs/spec.md:1151`. A hosted reranker is configured data
  egress reported in freshness evidence — acceptable as an option, wrong as
  default.

Estimate: one to two weeks of careful work, not an afternoon.

### Evaluability

Its main advantage. NDCG@k over existing graded lineage roots, reranker on
versus off, same corpora, same judgments — **no new eval cases needed**. Clears
the "earns its place in evaluation" gate on day one.

## Option B — QMD As A Source (integrate)

### What it is

`github.com/tobi/qmd`, already named in `docs/spec.md:1287` as the intended
second document backend. Verified 2026-07-29:

- TypeScript; requires Node ≥22 or Bun ≥1.0
- BM25 via SQLite FTS5, vector search, RRF hybrid fusion, `qwen3-reranker-0.6b`
  reranking, plus LLM query expansion
- ~2GB of GGUF models (embedding, reranker, expansion)
- Index in SQLite (FTS5 + vector extension) at `~/.cache/qmd/index.sqlite`
- CLI with `--json`; MIT; actively developed; also ships an MCP server

### The case for it

Skips rerank, dense, and hybrid at once. Integration shape is already solved in
this repo — `internal/adapters/tasks` and `internal/adapters/td` are CLI shells
with `cli.go` + `replay.go` fixture capture. `go.mod` stays at three
dependencies. No cgo, no determinism engineering inside our process, no model
distribution we own. Shortest path to good retrieval by a wide margin.

### Costs, worst first

1. **Unattributable results.** Query expansion *and* hybrid fusion *and*
   reranking in one shot. If dentist-001 is fixed, we do not learn which layer
   fixed it; if a smoke case regresses, we cannot attribute that either. Every
   design note in `search.go` exists because a behavior was measured and
   attributed. This makes "each layer earns its place" unenforceable exactly
   when it matters.
2. **Contract obligations may not survive.** Open questions, each a blocker if
   the answer is no:
   - Can document identity be reconstructed per chunk? `source_record_id` is the
     *document's* identity, because corroboration collapses on it and a
     per-chunk value lets one source corroborate itself with two halves of a
     paragraph.
   - Index Obligations (`docs/spec.md:829`) require a generation built fresh and
     published by pointer rename, with the prior generation readable and
     reported stale on failure. QMD owns one SQLite file. Does a failed reindex
     leave prior state queryable? Can a generation id and watermark be reported
     at all? Not patchable from outside the process.
   - `index_config` exists so a scoring change cannot be invisible. QMD's models
     and blending can change on a package bump — the exact thing that field
     prevents.
   - `Relevance` is the one cross-source comparable number. QMD returns an RRF
     score, which is ordinal and uncalibrated — what `internal/app` forbids
     thresholding on. So `RelevanceFloor` either stops working for this source,
     or we recompute relevance lexically over returned text, meaning we run our
     own scoring on top of theirs anyway.
3. **Determinism is worse than Option A.** LLM query expansion puts
   nondeterminism *upstream* of retrieval, in a process where we cannot quantize
   scores into our tie-break chain. Only lever is the replay-fixture pattern,
   which pins recorded output rather than proving reproducibility.
4. **Heaviest dependency, on the most fundamental source.** Node/Bun + ~2GB
   models + SQLite vector extension, as a hard requirement for *documents*. If
   QMD replaces `docs`, an unsatisfied dependency means Recall cannot search a
   directory at all. Compare Option A's sidecar: one runtime, sub-gigabyte
   model, lexical path still works without it.
5. **Third-party drift and supply chain** on the primary source path.

## Recommendation

**Do QMD first, as an additional source — not as a replacement.** Run it
alongside the built-in docs adapter over the same corpus, which is what the
adapter contract and multi-source fusion are for.

Rationale:

- The lexical baseline stays intact and stays the fallback.
- The eval packs compare them head-to-head on the same corpora with existing
  judgments — the comparison `docs/spec.md:1290` was written to enable.
- It answers the original question empirically *on this corpus*, for the price
  of one CLI adapter, before committing to build anything.
- The contract questions in Option B become findings rather than blockers, with
  a working system either way.

This reverses an earlier instinct to build the reranker first: QMD-as-second-
source is cheaper *and* it tells us whether the reranker is worth building.

What QMD is **not**: a replacement for the docs adapter, or the fix for the
dentist defect. That remains a relevance-definition bug, and Option A is still
the answer to it — probably as a follow-on once QMD data is in hand.

Do **not** put an embedding model inside `internal/adapters/docs`. That is the
one version of this that makes the project strictly worse: it destroys the
baseline the first document adapter exists to be.

## Cheapest Way To Kill This Early

Run `qmd` against a real corpus and inspect a single result for whether it can
fill a `recall.Candidate`:

- document identity distinct from chunk identity
- a **line range**, without which `expand` breaks and the integration is a much
  larger job than it looks
- something that can honestly become `excerpt_kind` (matched versus preview)

If the line range is absent, reconsider the whole option before writing adapter
code.

## Open Questions For The Decision

1. Does QMD expose line ranges and stable document identity? (Blocker; check
   first.)
2. Is a ~2GB model download plus a Node/Bun runtime acceptable as an *optional*
   source dependency? (It is not acceptable as a required one.)
3. Do we accept replay fixtures as the determinism story for a nondeterministic
   source, given byte-exact README transcripts and pack content hashing?
4. If Option A follows: amend `docs/spec.md:1237` to admit a non-source adapter
   role for reranking?
5. Which eval cases get added for paraphrase recall? Neither `smoke` nor
   `shapes` currently contains a question whose answer shares almost no surface
   terms with it — so the gap in "The Gap That Is Real" is currently
   unmeasurable, whichever option is chosen.

## Spike Findings (2026-07-29, td-89cab3) — GO

qmd 2.5.3 (`bun install -g @tobilu/qmd`), indexed over `~/code/clara-home`
(58 `.md` files, 188 chunks), models relocated to `/Volumes/Swift/qmd-cache`
via `~/.cache/qmd` symlink (334M embeddinggemma-300M-Q8_0 + 639M
qwen3-reranker-0.6b-q8_0 + 1.3G qmd-query-expansion-1.7B-q4_k_m ≈ 2.1G).

1. **Line ranges: present.** Every JSON result carries `line` (span start) and
   the snippet header encodes the span: `@@ -17,4 @@ (16 before, 88 after)`.
   `qmd get <path>:<from>:<count>` returns line-numbered content. Locator
   `<path>#La-Lb` and `expand` are both satisfiable. Blocker cleared.
2. **Identity: file path per collection** (`qmd://clara-home/<relpath>`),
   distinct from `docid` (content hash). `source_record_id` = relpath, same
   value the docs adapter derives for the same file → cross-source
   corroboration collapse works by construction.
3. **Layer control: native.** `search` (BM25 only), `vsearch` (vector only),
   `query --no-rerank` (hybrid, no reranker), `query` (full). A structured
   query document (`lex:`/`vec:` lines) bypasses LLM expansion entirely.
   `--explain` returns per-component evidence: every expanded query string,
   its source (original/lex/vec/hyde), per-list RRF rank/weight/contribution,
   raw FTS and vector scores, rerank score. Attributability is fully served.
4. **Determinism: better than planned.** Warm repeated queries are
   byte-identical (result cache). Cold first-run writes spinner/download
   progress to **stdout**, corrupting `--json` — the adapter must pre-warm
   via refresh and reject non-JSON stdout prefixes.
5. **The honesty risk is real and measured.** Off-corpus query ("sourdough
   starter hydration recipe") → full pipeline returns a Pendleton travel
   guide at rerank score **0.88** — the same score a genuinely-relevant
   result earns. `--min-score` cannot express an admission floor; the
   reranker saturates. BM25 `search` honestly returns `[]` (exit 0).
   Consequence: relevance/admission must be recomputed lexically on our
   side; qmd scores are ordering-only. This confirms the plan's central
   design constraint rather than weakening it.
6. **Ops.** Warm query ~0.7s wall on this corpus; `embed` 58 docs in 14s;
   index at `~/.cache/qmd/index.sqlite` (5MB); `QMD_CONFIG_DIR` relocates
   config; exit code is 0 even for empty results (outcome mapping is ours).
   Incremental `update`; failed-reindex semantics untested — verify during
   implementation (index obligations reporting is best-effort: sqlite mtime
   as watermark, qmd version + model filenames + mode as `index_config`).

## Design Decisions (td-427790)

1. **Shape: first-party external adapter `cmd/recall-qmd`** (adapter.Serve
   over stdio, recall-gmail precedent), shelling to `qmd` with td-style
   runner hardening (read-only subcommand allowlist, `--` before free text,
   ANSI scrub, replay runner via the `replay` settings key). Built-in
   rejected: td's built-in criterion (shared across installs) does not hold;
   external gets `recall doctor --conformance qmd`.
2. **Relevance: basis depends on the mode** — never nil either way (nil reads
   as 1.0). qmd's score still goes to `LocalScore` and qmd's ordering to
   `LocalRank`; `--explain` components go into diagnostics.

   - `hybrid` and `full`: **recomputed lexically** over qmd-returned text via
     `recall.RelevanceOverCounts`, as originally decided.
   - `vector`: **qmd's cosine similarity**, quantized to four fixed decimals
     and clamped to [0,1], reported as `relevance_basis =
     "vector_similarity"` in the search diagnostics.

   **Amended 2026-07-29 after the live rollout (td-09f45f, td-74110d).** The
   original decision was right about `hybrid`/`full` and wrong about
   `vector`, and both halves were settled by measurement on the clara-home
   corpus rather than by argument.

   The plan's objection — "QMD returns an RRF score, which is ordinal and
   uncalibrated" — holds exactly for `query`/`query --no-rerank`. Those
   scores are RANK-NORMALIZED: rank 1 is 1.0 whatever matched, and the noise
   queries `kodachrome` and `zxqv` come back at 1.0 / 0.5 / 0.33 down a list
   of unrelated documents. There is no admission information in them, so
   those modes keep the lexical recompute. It also follows that `hybrid` and
   `full` have NO admission floor of their own and can flip an abstention
   into a confident answer — which is why `vector` is now the recommended
   mode beside a lexical source, reinforced by latency (`full` 10.65s against
   the 5s `DefaultQueryBudget`; `vector` ~2.4s) and by the observation that
   Recall's own fusion already IS the hybrid layer.

   `vsearch` cosine is a different quantity and the objection does not
   transfer. It is bounded, it means the same thing for every query, and it
   separates: true paraphrases score 0.42–0.45, while `kodachrome` and `zxqv`
   do not clear qmd's own floor and return an empty list — so the honest
   empty result arrives before any number is reported. "Who takes care of my
   teeth" returns exactly the dentist record at 0.45.

   What forced the amendment is that finding 5's conclusion, applied to
   `vector`, inverted the epic's purpose. A lexical measure cannot see
   paraphrase aboutness — coverage times concentration over shared query terms
   is 0 when the question and the answer share no words — so the recomputed
   relevance was ~0 on every true paraphrase and the core's relevance floor
   withheld the dentist candidate with `below_relevance_floor`, verified on
   the live profile. The corpus held the answer, qmd found it, and it was
   suppressed for not repeating the question's words. Noise-robustness, which
   is what the lexical recompute was protecting, is already guaranteed
   upstream by qmd's own cosine floor.
3. **Identity:** `source_record_id` = collection-relative path (docs-adapter
   parity); `content_fingerprint` from qmd `docid` where present.
4. **Locator/expand:** `<relpath>#La-Lb` from `line` + span; `expand` reads
   the file directly (docs precedent) — qmd is not on the expand path.
5. **Index obligations:** indexing only via `recall/refresh` (`qmd update` +
   `qmd embed`, then one throwaway warm query to force model load);
   `index_config` = {qmd version, model filenames, mode}; watermark =
   index.sqlite mtime; generation reporting best-effort and declared.
6. **Failure taxonomy:** missing binary/runtime → spawn_failed; non-JSON or
   pre-JSON noise on stdout → broken contract (`invalid_response`); timeout →
   deadline_exceeded. All degrade coverage (exit 3), source named.
7. **Sensitivity/egress:** default `sensitivity = "internal"`; qmd is local
   inference only (verified: no network at query time once models cached);
   model download happens at install/refresh, documented as the one egress.
8. **Modes** (td-f63dca): setting `mode = bm25 | vector | hybrid | full`
   mapping to search/vsearch/query--no-rerank/query; recorded in
   `index_config`; committed eval packs use replay fixtures regardless.

# Documents adapter: expose the OKF trust tier

- **Task:** td-f8015a (this repo's td store)
- **Origin:** proposed during clara-home's OKF adoption
  (`~/code/clara-home/docs/plans/active/open-knowledge-format.md`, Phase 3);
  approved by Marcus as a phase, not requested directly.
- **Status:** planned. No implementation started.

## Outcome

The documents adapter derives each document's Open Knowledge Format trust tier
from its frontmatter and reports it on every candidate it returns. A consumer —
today a hypothetical profile rule, tomorrow any agent reading `--json` — can
then prefer human-reviewed documents without re-parsing frontmatter itself.
Ranking is untouched.

## Background

OKF v0.2 (GoogleCloudPlatform/knowledge-catalog, `okf/SPEC.md`) puts trust in
frontmatter: `generated: {by, at}` records who wrote the current content;
`verified: [{by, at}]` records who confirmed it. Actors follow §7:
`human:<id>`, `<producer>/<version>` (e.g. `codex/gpt-5`), `process:<id>`.

Tier derivation is §5.3 and entirely mechanical:

- no `verified` key ⇒ **unverified**
- `verified` only by non-`human:` actors ⇒ **machine-confirmed**
- `verified` including a `human:<id>` actor ⇒ **human-reviewed**

A single verifier MAY be written as a bare mapping rather than a list; consumers
MUST treat it as one element (§5.2). Conformance (§11) requires consumers to
tolerate missing, malformed, and unknown frontmatter — a bad block must never
make a document unindexable or rejected.

clara-home now carries OKF v0.2 frontmatter on its first-class markdown,
including `verified: [{by: human:marcus, ...}]` on reviewed docs. Today the
adapter indexes that frontmatter as plain preamble text (`parseChunks` folds it
into chunk 0's body) and nothing consumes the signal.

## Settled decisions

1. **Derive at build time, store on the document.** `readDocument`
   (`internal/adapters/docs/index.go:412`) parses the frontmatter block and
   stores the tier on a new `indexedDoc.TrustTier`. The tier is a fact about
   the *document*; chunks repeat it via the existing `g.doc(c.Path)` lookup in
   `candidates()` rather than carrying their own copy, the same way `Project`
   and `Title` travel today.

2. **Bump `indexFormat` 3 → 4.** Old generations decode fine but report no
   tier, and the alternative to a forced rebuild is two profiles disagreeing
   about a document's trust depending on when each source was last built —
   exactly the inconsistency the format rule exists to prevent (see the const
   comment at `index.go:34-43`). Cost is one rebuild per source. No settings
   change, so the settings digest is unaffected; the bump rides the existing
   format-mismatch rebuild path.

3. **Hand-rolled bounded frontmatter scanner; no YAML dependency.** `go.mod` is
   deliberately toml-plus-jsonschema, and this package already hand-rolls its
   Markdown primitives (`atxHeading`, `fenceMarker`). The grammar the trust
   family needs is tiny and enumerable: detect a leading `---` … `---` block,
   find the `verified` key, collect `by:` actor values from the bare-mapping,
   flow-list, and block-list shapes. Anything that does not parse cleanly falls
   back to unverified and never fails the document. A general YAML library buys
   robustness we do not need at the price of a dependency stance this repo has
   so far declined.

4. **Actor-shape validation guards against false positives.** A `by:` value
   counts as a verifier only if it looks like an actor: `human:` prefix,
   `process:` prefix, or a `<producer>/<version>` shape. This keeps prose like
   a multi-line `description:` containing "by:" from manufacturing a
   verification event. Values are matched, never validated further — an
   unknown-but-actor-shaped string is still a non-human verifier.

5. **Always emit the tier; absence is not a fourth state.** After the format
   bump every generation can answer, and §5.3 makes the answer for
   frontmatter-less documents "unverified". Omitting the key would force every
   consumer to distinguish "source did not say" from "unverified", which is a
   distinction this adapter has no honest basis for.

6. **Wire vocabulary: `Metadata["trust_tier"]` with spec-exact values.** The
   candidate envelope's `metadata` is free-form
   (`pkg/protocol/schemas/search_result.json`, `$defs.candidate.metadata`),
   so this needs no schema change and no additive-field policy debate — unlike
   a top-level field, which would be REJECTED by older cores and touch the
   unresolved gap documented in `docs/adapter-protocol.md` (td-e7a59e).
   Key spelling follows sibling metadata keys (`heading_path`, `doc_title`);
   values are the §5.3 names verbatim: `unverified`,
   `machine-confirmed`, `human-reviewed`.

7. **`TrustTier` lives in `pkg/recall/enums.go` as a string type**, following
   the `ExcerptKind` shape. Rationale: the first consumers that will want to
   validate tier names (pointer projection, a future profile knob in
   `internal/config`/`internal/ranking`) already import `pkg/recall` and may
   not import `internal/adapters/docs`. It is deliberately NOT added to the
   candidate struct or protocol schemas (decision 6).

8. **Pointer tier gains `Result.TrustTier` (omitempty).** The pointer
   projection keeps facts that are claims about the evidence — that is why
   `excerpt_kind` survives. Verification state is such a fact: an agent
   choosing which locator to expand or cite is exactly the reader who needs
   it, and the pointer tier is the agent surface. Projected from
   `Primary.Metadata`; costs ~25 bytes per result. Reversible without
   migration since it is additive and omitempty.

9. **Human CLI output unchanged.** Most corpora today are unverified; printing
   a marker per result is noise until someone asks for it. `--json --explain`
   carries the full candidate including metadata already.

10. **Retrieval-neutral.** Frontmatter continues to be indexed as preamble text
    exactly as today. Removing it from bodies/terms would shift BM25 lengths
    and density for every frontmatter-bearing document — a measurable ranking
    change that must earn its place in evaluation, not ride along here.

## Work sequence

Each step compiles and tests green on its own; steps 1–2 are pure additions
with no callers yet.

### 1. Vocabulary: `TrustTier` in `pkg/recall`

- `pkg/recall/enums.go`: add
  `type TrustTier string` with `TrustUnverified`, `TrustMachineConfirmed`,
  `TrustHumanReviewed` constants whose values are the §5.3 names, mirroring the
  local `ExcerptKind` conventions (String, validity helper if the file's other
  string enums carry one).
- Table tests beside the existing enum tests: constant values spell the spec
  names exactly.

### 2. Scanner: `internal/adapters/docs/frontmatter.go` (new)

- `splitFrontmatter(lines []string) (block []string, rest []string, ok bool)` —
  recognizes only a `---` on the file's first line with a closing `---`; a
  missing closer means "no frontmatter", not an error.
- `trustTier(block []string) recall.TrustTier` — walks the block tracking the
  current top-level key so `generated.by` never counts as verification; accepts
  the three §5.2 written forms (bare mapping, flow list, block list) plus
  block-style `- by: …` items; applies the actor-shape rule; returns
  human-reviewed > machine-confirmed > unverified.
- Tolerance rules, tested explicitly: malformed YAML, unterminated braces,
  unknown keys, empty `by:` values, comments — all yield unverified, and none
  can panic. Long blocks are bounded (scanner stops at a generous fixed line
  cap) so a pathological file cannot dominate a build.
- `frontmatter_test.go`: table tests over every shape in OKF §5.2, the
  worked examples in the spec appendix, plus the tolerance cases above, and a
  `generated`-only document asserting it stays unverified.

### 3. Index plumbing: `internal/adapters/docs/index.go`

- Add `TrustTier recall.TrustTier \`json:"trust_tier"\`` to `indexedDoc`.
- `readDocument`: split frontmatter off `lines` before `parseChunks`, derive
  the tier, set it on the doc. `rest` (not `lines`) feeds `parseChunks` — the
  chunk ordinals, line numbers, and therefore locators must stay stable, which
  means compensating for the removed delimiter lines when computing
  StartLine/EndLine. Simplest correct approach: pass the full `lines` to
  `parseChunks` as today and instead have the scanner consume only what it
  needs, leaving chunking input untouched; choose whichever keeps locators
  byte-identical with format-3 generations and assert it in a test.
- Bump `indexFormat` to 4 and extend the const-block comment with the format-4
  rationale (one sentence, house style: why a stale generation must not keep
  answering).
- Builder test: a corpus file with each tier's frontmatter round-trips through
  a generation; a format-4 build rejects a synthesized format-3 header (existing
  mismatch path asserts the rebuild trigger).

### 4. Surfacing: `candidates()` and pointer projection

- `search.go` `candidates()`: add `"trust_tier": doc.TrustTier` to the
  Metadata map (decision 5: present on every candidate).
- `internal/pointer/pointer.go`: add `TrustTier recall.TrustTier
  \`json:"trust_tier,omitempty"\`` to `Result` and project it from
  `r.Primary.Metadata["trust_tier"]` in `ProjectResult`. A typed cast with a
  nil/unknown-safe fallback; metadata from other adapters simply yields "".
- Tests: candidates carry the expected tier per fixture document; pointer
  projection test alongside the existing ones in `internal/cli/pointer_test.go`.

Fixtures: write frontmatter-bearing documents in `t.TempDir()` corpora inside
the tests rather than extending the shared `testdata/corpus`, so counts and
golden expectations of existing tests stay untouched.

### 5. Docs and changelog

- Wherever the docs source class describes per-record metadata (the
  **Document corpora** section of `docs/spec.md`, ~line 786), add the derived
  field and the OKF §5.3 derivation in two sentences.
- `CHANGELOG.md`: one entry per its existing format.

### 6. Verification

- `make check` (build + tests + lint) green; targeted suites:
  `go test ./internal/adapters/docs/ ./pkg/recall/ ./internal/pointer/
  ./internal/cli/`.
- **Ordering invariant:** the change must not move a single result. On the
  live clara-home corpus, run a fixed set of queries before and after
  (checkout stash or worktree) and diff the ranked locator lists — they must be
  byte-identical. This is the executable form of decision 10.
- **Live tier check:** against the same corpus, confirm documents known to
  carry `verified: [{by: human:marcus, …}]` report
  `results[].trust_tier == "human-reviewed"` in `recall query --json`, and
  plain docs report `unverified`. Respect the exit-code contract when
  scripting the check (2 is abstention, not failure).
- `make eval` is not required by this change (no scoring path touched); run
  it only if step 3's locator-stability assertion forces any chunking edit.

## Non-goals

- **No ranking change, no profile knob.** The prior-bump consumer is a later
  piece of work in `internal/ranking`/`internal/config` (the natural seam is
  beside the per-source priors in `ranking.Config`); this task only makes the
  signal available. File it separately if Marcus wants it now.
- **No frontmatter extraction beyond `verified`.** `generated`, `status`,
  `stale_after`, `description`, and `tags` stay unparsed; the scanner's
  top-level-key tracking leaves room for them without committing to anything.
- **No protocol/schema changes,** no new manifest capability, no settings.
- **No stripping of frontmatter from the indexed text.**
- **No expansion-surface changes:** `ExpandResponse` stays content +
  provenance.

## Risks

| Risk | Mitigation |
|---|---|
| Hand-rolled parser misreads a legal YAML shape | Enumerate the spec's shapes in tests; unknown shapes fail safe to unverified; §11 makes tolerance the required behavior anyway |
| Locator drift from touching `readDocument` | Decide chunk-input question by test (step 3): locators must stay identical to format 3 |
| Stale generations answering without tiers | Format bump forces one rebuild everywhere; health shows freshness |
| False verification from prose-shaped `by:` values | Actor-shape gate; worst case is a wrong tier, never a crash or an unindexable doc |
| Scope creep toward a general frontmatter layer | Non-goals are binding; each addition must name its consumer first |

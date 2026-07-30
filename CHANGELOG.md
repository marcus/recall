# Changelog

All notable changes to Recall are documented here.

Before 1.0, minor releases may contain breaking changes to the public Go
adapter SDK under `pkg/`. Each such change will be called out here with
migration guidance.

## [0.3.0] - 2026-07-29

### Breaking

- Every adapter's manifest must now declare `relevance_basis` — the quantity it
  places in `Candidate.Relevance`, either `lexical_span` or
  `vector_similarity`. The wire schema requires the field, so an external
  adapter that does not send it fails its handshake with
  `recall/initialize result: at / (/required)` and its source is reported
  unavailable. Migration: Go adapters set
  `Manifest.RelevanceBasis: recall.RelevanceLexicalSpan` (the right value for
  any lexical source) and rebuild against the current `pkg/recall`; other
  adapters add `"relevance_basis": "lexical_span"` to the manifest in their
  `recall/initialize` result. Recorded conformance transcripts embed the
  manifest, so re-record them with your adapter's recorder afterward. The
  declaration exists because relevance values on different bases are admissible
  against the floor but not comparable across sources, and that difference was
  previously invisible to configuration, evaluation, and `--explain`.

### Added

- Evaluation reports group source families by their declared relevance basis,
  and `by_source_family` metrics name it, so a comparison across bases is
  visibly a comparison across bases.

### Fixed

- The declared relevance basis survives report round-trips and budget
  compression rather than being dropped with diagnostics.
- The Homebrew formula template builds and tests `recall-qmd` and installs its
  conformance transcripts; `brew audit --strict` passes.
- `docs/qmd-adapter.md` reads without project context: the relevance-definition
  problem it references is explained in place, and measured numbers are scoped
  to the corpus they came from.

## [0.2.0] - 2026-07-29

### Added

- Added the optional first-party `recall-qmd` external adapter. It exposes a
  [qmd](https://github.com/tobi/qmd) collection as a semantic document source
  running *alongside* the built-in lexical adapter over the same corpus, with a
  `mode` setting isolating qmd's full-text, vector, hybrid, and reranked layers
  and recording the choice in `index_config`. Relevance is recomputed on
  Recall's shared definition rather than taken from qmd's saturating score,
  document identity matches what the lexical adapter derives for the same file,
  and every operation re-verifies that the configured collection still indexes
  the configured corpus. See [qmd-adapter.md](docs/qmd-adapter.md).
- The `excerpt_kind` candidate field is now accepted over the wire. It was
  documented in the adapter protocol and present in `pkg/recall`, but absent
  from `search_result.json`, so an external adapter that emitted it was rejected
  by schema validation rather than degraded.
- Added the optional first-party `recall-gmail` external adapter. It reads
  Gmail through `gog`, ships synthetic conformance transcripts, excludes bulk
  categories from its default corpus, keeps message bodies out of pointers,
  strips URLs during expansion, and raises credential-shaped mail to
  `restricted`.

## [0.1.0] - 2026-07-28

### Added

- Added the `recall` CLI, HTTP API, and MCP server for federated retrieval
  across user-controlled document, Tasks, and `td` sources.
- Added pointer-first query and expansion, source refresh, health diagnostics,
  configuration explanation, and deterministic `recall init --docs` first-run
  setup.
- Published the external adapter protocol, the `recall-stream` reference
  adapter, recorded conformance testing, and the public Go adapter SDK at
  `pkg/adapter`, `pkg/protocol`, `pkg/recall`, `pkg/conformance`, and
  `pkg/buildinfo`.
- Added deterministic smoke and shapes evaluation packs with committed
  baselines for retrieval, coverage, provenance, suppression, and abstention.

### Changed

- Private-source adapters and real-corpus evaluation fixtures now live with
  their private consumers. The public repository carries only reusable
  adapters and synthetic evaluation data.

### Security

- Project configuration cannot introduce executable adapter commands; commands
  must come from user-level configuration.
- Query results preserve source sensitivity ceilings and distinguish complete
  abstention from degraded or failed coverage.

[Unreleased]: https://github.com/marcus/recall/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/marcus/recall/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/marcus/recall/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/marcus/recall/releases/tag/v0.1.0

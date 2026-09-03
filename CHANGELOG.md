# Changelog

All notable changes to Recall are documented here.

Before 1.0, minor releases may contain breaking changes to the public Go
adapter SDK under `pkg/`. Each such change will be called out here with
migration guidance.

## [Unreleased]

### Added

- `recall sidecar-plugin` answers the Sidecar plugin protocol
  (`sidecar.plugin/v1-draft`): one JSON request on stdin, one JSON response on
  stdout, one process per call. It offers a `results` collection over `recall
  query`, a `sources` collection over `recall sources`, documents for both, and
  a confirmed `refresh-source` action, and it reads Sidecar's project context as
  a `--scope project=` narrowing. Recall's `failed` outcome has no page outcome
  in the draft protocol and comes back as a retryable `unavailable` error. See
  the README's "Sidecar plugin" section for the config entry and the checks.

## [0.5.1] - 2026-08-26

### Changed

- The module and documented toolchain are Go 1.27.0. CI reads the version from
  `go.mod`, and `make lint` fails closed unless the local `golangci-lint` is
  v2.13.1, the first release that typechecks 1.27.

### Fixed

- `recall refresh --source` on a live adapter that cannot checkpoint (td,
  tasks) now probes that source's health instead of exiting 4 with
  `checkpoint_unsupported`. A healthy live source exits 0 with health
  attached; an unhealthy one exits non-zero with the same diagnostic
  `recall sources` already prints. Profile-wide `recall refresh` still skips
  live sources.

## [0.5.0] - 2026-08-11

### Added

- `[defaults] budget_ms` sets the end-to-end request latency budget when a
  caller omits one (`recall query` without `--budget-ms`, and the same on
  HTTP/MCP). Raising `timeout_ms` alone never did this: that key remains the
  per-source inheritance default. Machines that need a longer cold-start
  ceiling for vector sources should set `budget_ms` explicitly; unset keeps
  the engine's 5s fallback.

## [0.4.0] - 2026-07-30

### Added

- Adapter manifests can declare effective external executable requirements.
  `recall sources` exposes them in both output tiers, and `recall doctor`
  preflights the declared command without hardcoding adapter-specific tools.
  The first-party qmd and Gmail adapters declare their configured `qmd` and
  `gog` commands respectively; replay-backed instances declare none.
- Timed-out source reports now identify whether the end-to-end request latency
  budget or the configured per-source timeout supplied the deadline, and say
  `adapter_internal` when an adapter's own earlier deadline fired. Caller
  cancellation is reported separately and never blamed on either timeout, and
  an earlier outer context deadline is attributed as `caller_deadline`.

### Fixed

- `recall refresh` now prints the actionable health diagnostic detail it
  already returned in structured health, and exposes the same detail as the
  typed `diagnostic_detail` field.
- A successful qmd refresh whose comparable file, document, and vector counts
  move only forward is reported as refreshed even if a concurrently changing
  corpus leaves its attached health partial. The health remains degraded for
  query honesty; unchanged, regressed, failed, and incomparable checkpoints
  still degrade the refresh.
- qmd slash-containing relative binary settings now resolve against the corpus
  working directory before both execution and manifest declaration, so
  `recall doctor` preflights the same command qmd will run.

## [0.3.1] - 2026-07-30

### Fixed

- `recall-gmail` now reports `relevance` on every candidate, measured over the
  sender and subject span it actually holds, including zero. It previously
  omitted the number whenever the header covered none of the query terms —
  which is most conversational queries, because Gmail matches server-side
  against bodies a pointer-first adapter never fetches — and an omitted
  relevance is fused as 1.0, the maximum. Mail threads therefore outranked
  person, calendar, and task records that had measured themselves honestly. A
  low mail relevance now means only that the visible header holds nothing of the
  query, which is what the declared `lexical_span` basis says it measures; see
  [gmail-adapter.md](docs/gmail-adapter.md).

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

[Unreleased]: https://github.com/marcus/recall/compare/v0.5.1...HEAD
[0.5.1]: https://github.com/marcus/recall/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/marcus/recall/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/marcus/recall/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/marcus/recall/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/marcus/recall/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/marcus/recall/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/marcus/recall/releases/tag/v0.1.0

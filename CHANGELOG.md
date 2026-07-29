# Changelog

All notable changes to Recall are documented here.

Before 1.0, minor releases may contain breaking changes to the public Go
adapter SDK under `pkg/`. Each such change will be called out here with
migration guidance.

## Unreleased

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

[Unreleased]: https://github.com/marcus/recall/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/marcus/recall/releases/tag/v0.1.0

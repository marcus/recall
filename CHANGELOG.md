# Changelog

All notable changes to Recall are documented here.

Before 1.0, minor releases may contain breaking changes to the public Go
adapter SDK under `pkg/`. Each such change will be called out here with
migration guidance.

## Unreleased

### Changed

- Published the Go adapter SDK at `pkg/adapter`, `pkg/protocol`, `pkg/recall`,
  `pkg/conformance`, and `pkg/buildinfo`. Existing in-tree imports move from
  `github.com/marcus/recall/internal/<package>` to
  `github.com/marcus/recall/pkg/<package>`; protocol behavior is unchanged.

# Plan: Upgrade to Go 1.27

Status: implemented.

## Goal

Move recall from Go 1.26.4 to Go 1.27.0, and along the way fix the two pieces of CI hygiene this upgrade exposes: hardcoded workflow Go versions and a golangci-lint pin that predates Go 1.27 support. Recall adds one verification surface no other project here has: the eval harness with committed performance baselines.

## Current state

| Concern | Today |
|---|---|
| go.mod directive | `go 1.27.0`, no `toolchain` directive |
| ci.yml | `go-version-file: go.mod` in every job; runs `make eval`; golangci-lint-action pinned at **v2.13.1** |
| release.yml | `go-version-file: go.mod`; golangci-lint-action v2.13.1; goreleaser v2.16.0 (install-only) |
| Makefile | `lint:` guards the local binary against `GOLANGCI_LINT_VERSION ?= v2.13.1` |
| Eval | Committed baselines unchanged: `make eval` compared clean, no metric movement |
| Plans convention | this directory (`docs/plans/implemented`) |

v2.12.2 predates Go 1.27 support (added in golangci-lint v2.13.0): its bundled typechecker rejects the new stdlib and its staticcheck panics on generic methods. sidecar hit this exact failure mode when Homebrew shipped Go 1.27.0 and fixed it by moving to v2.13.1 — recall is exposed both locally (unversioned binary) and in CI (pinned v2.12.2) the moment anything builds with 1.27.

## What Go 1.27 changes that touch this repo

- **encoding/json/v2 is now the default implementation** (escape hatch: `GOEXPERIMENT=nojsonv2`). Error strings differ in places; corpus/baseline parsing and any tests asserting exact error text are the watch list.
- **Runtime allocation got faster** — good news that still moves numbers. The eval gate compares against committed baselines, so expected improvements need a deliberate re-baseline, not silence.
- **stdversion vet** runs by default under `go test`.
- Generic methods / embedded field selectors are legal syntax — which is precisely what old staticcheck panics on.
- darwin floor stays macOS 13+.
- **net/http shutdown of an incomplete request** waits for ReadTimeout, the stdlib's 500ms RST-avoidance pause, and Shutdown poll backoff. That sum exceeds one second; the serve drain test bound is 3s, still inside the 5s Shutdown fallback.

## Work sequence

1. **CI hygiene first**: replace every hardcoded `go-version:` with `go-version-file: go.mod` in both workflows; bump golangci-lint-action to `version: v2.13.1` everywhere it appears.
2. **Local linter**: rebuild the local binary at v2.13.1 (`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1`) so `make lint` works again; optionally add sidecar-style version verification to the `lint` target so drift is loud next time.
3. **Directive**: `go.mod` → `go 1.27.0`, then `go mod tidy`.
4. **Tests**: full suite; bisect any JSON-related failure with `GOEXPERIMENT=nojsonv2` before changing code.
5. **Eval re-baseline**: run `make eval`, review `recall eval compare` output against committed baselines. Improvements get a deliberate baseline refresh commit with the upgrade as justification; regressions get investigated before proceeding, not papered over.
6. **Release rehearsal**: `goreleaser release --snapshot --clean`; confirm packaging targets build under 1.27.

## Coordination

Recall is standalone — no go.work coupling to td/tasks/sidecar. Land it independently in any order relative to the other three upgrades.

## Verification & acceptance evidence

- Both workflows contain no hardcoded Go versions; lint action at v2.13.1.
- `go.mod` is `go 1.27.0`. README, CONTRIBUTING, and docs/quickstart state Go 1.27.0; CONTRIBUTING states golangci-lint 2.13.1.
- `make check` (build, race-enabled tests, golangci-lint 2.13.1) clean locally. No JSON error-string failures.
- `make eval`: all three packs pass; compare reported **No regression** and no metric movement; baselines not refreshed.
- `make release-snapshot` built darwin/linux amd64+arm64 archives and passed archive and publication guards.

Out of scope: adopting json/v2-specific APIs, dependency upgrades beyond tidy, changing what the eval harness measures.

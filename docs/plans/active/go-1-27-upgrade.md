# Plan: Upgrade to Go 1.27

## Goal

Move recall from Go 1.26.4 to Go 1.27.0, and along the way fix the two pieces of CI hygiene this upgrade exposes: hardcoded workflow Go versions and a golangci-lint pin that predates Go 1.27 support. Recall adds one verification surface no other project here has: the eval harness with committed performance baselines.

## Current state

| Concern | Today |
|---|---|
| go.mod directive | `go 1.26.4`, no `toolchain` directive |
| ci.yml | Hardcoded `go-version: '1.26'` in multiple jobs (including release-shape) instead of `go-version-file: go.mod`; runs `make eval`; golangci-lint-action pinned at **v2.12.2** |
| release.yml | Mix of `go-version-file` and hardcoded versions; golangci-lint-action also v2.12.2; goreleaser v2.16.0 (install-only) |
| Makefile | `lint:` runs bare `golangci-lint run` with no version guard (sidecar's `make lint` shows the pattern for one) |
| Eval | Committed baselines; `recall eval compare` exits non-zero when a measurement regresses against them, so drift fails CI rather than hiding |
| Plans convention | this directory (`docs/plans/active`) |

v2.12.2 predates Go 1.27 support (added in golangci-lint v2.13.0): its bundled typechecker rejects the new stdlib and its staticcheck panics on generic methods. sidecar hit this exact failure mode when Homebrew shipped Go 1.27.0 and fixed it by moving to v2.13.1 — recall is exposed both locally (unversioned binary) and in CI (pinned v2.12.2) the moment anything builds with 1.27.

## What Go 1.27 changes that touch this repo

- **encoding/json/v2 is now the default implementation** (escape hatch: `GOEXPERIMENT=nojsonv2`). Error strings differ in places; corpus/baseline parsing and any tests asserting exact error text are the watch list.
- **Runtime allocation got faster** — good news that still moves numbers. The eval gate compares against committed baselines, so expected improvements need a deliberate re-baseline, not silence.
- **stdversion vet** runs by default under `go test`.
- Generic methods / embedded field selectors are legal syntax — which is precisely what old staticcheck panics on.
- darwin floor stays macOS 13+.

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

- Both workflows contain no hardcoded Go versions; lint action at v2.13.1 green on CI.
- `go build ./...`, `go test ./...`, `golangci-lint run ./...` clean locally.
- Eval run recorded: compare output clean or baselines refreshed with an explanatory commit message.
- Snapshot artifacts build for all configured targets.

Out of scope: adopting json/v2-specific APIs, dependency upgrades beyond tidy, changing what the eval harness measures.

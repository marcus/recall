GO      ?= go
GORELEASER ?= goreleaser
PKG     := github.com/marcus/recall
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X $(PKG)/pkg/buildinfo.Version=$(VERSION) \
           -X $(PKG)/pkg/buildinfo.Commit=$(COMMIT)
BIN     := bin

PREFIX  ?= $(HOME)/.local
RELEASE_VERSION ?=
export RELEASE_VERSION

# RELEASE_VERSION must arrive as an environment variable, never as a command
# line override. A command line value is make data that later reaches shell
# recipes; requiring the environment keeps the version out of make's own
# variable expansion for the two goals that consume it directly.
release_goals := $(filter check-release-state release-tap,$(MAKECMDGOALS))
ifneq ($(release_goals),)
ifneq ($(origin RELEASE_VERSION),environment)
$(error set RELEASE_VERSION in the environment, for example: RELEASE_VERSION=v0.6.0 make $(firstword $(release_goals)))
endif
endif

.PHONY: all build build-all install uninstall test lint fmt cover clean tidy check \
	release-snapshot release-preflight release release-dry-run release-publish \
	release-tap check-release-state

all: check

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/recall ./cmd/recall

# Every command, not just the core CLI. External adapters are separate binaries
# that the core spawns by name, so a partial build leaves configured sources
# unreachable at runtime rather than at build time.
build-all:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/ ./cmd/...

test:
	$(GO) test -race -count=1 ./...

cover:
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

# Must match golangci-lint-action version in .github/workflows/ci.yml and
# .github/workflows/release.yml. A local binary older than the CI pin predates
# Go 1.27 support and fails in ways CI never sees; make the drift loud here.
GOLANGCI_LINT_VERSION ?= v2.13.1

lint:
	@got=$$(golangci-lint version 2>/dev/null | sed -n 's/^golangci-lint has version \([0-9.]*\).*/\1/p' | head -1); \
	want=$(patsubst v%,%,$(GOLANGCI_LINT_VERSION)); \
	if [ -z "$$got" ]; then \
		echo "golangci-lint is not installed (need $(GOLANGCI_LINT_VERSION))"; \
		exit 1; \
	fi; \
	if [ "$$got" != "$$want" ]; then \
		echo "golangci-lint v$$got != GitHub $(GOLANGCI_LINT_VERSION) (.github/workflows/ci.yml)"; \
		exit 1; \
	fi
	golangci-lint run

fmt:
	$(GO) fmt ./...
	gofmt -s -w .

tidy:
	$(GO) mod tidy

check: build test lint

# The command set comes from cmd/, never from the contents of bin/. bin/ is
# gitignored and no target prunes it, so a renamed or deleted command leaves a
# binary behind that would otherwise be installed forever, and `uninstall`
# would delete whatever happened to share its name on the user's PATH.
COMMANDS := $(notdir $(wildcard cmd/*))

# Adapter binaries must land on the same PATH as the CLI: the core spawns them
# by command name, so installing recall alone yields a config that passes
# validation and then fails to reach half its sources. Each iteration exits
# non-zero on failure — with `&&` the recipe reported success whenever the
# final command happened to install, which is exactly the partial install this
# target exists to prevent.
install: build-all
	@mkdir -p '$(PREFIX)/bin'
	@for c in $(COMMANDS); do \
		install -m 0755 '$(BIN)'/"$$c" '$(PREFIX)/bin/' || exit 1; \
		echo "installed $$c -> $(PREFIX)/bin"; \
	done

uninstall:
	@for c in $(COMMANDS); do \
		rm -f '$(PREFIX)/bin/'"$$c" || exit 1; \
		echo "removed $$c"; \
	done

clean:
	rm -rf $(BIN) dist coverage.out

# eval runs the committed packs through the same application layer the CLI
# uses, and compares each against its committed baseline. It is a separate
# target from `check` because it builds several binaries and spawns them; CI
# runs both.
#
# Every pack runs, and a failure in any of them fails the target. smoke covers
# the failure vocabulary a healthy corpus cannot produce; shapes is a compact
# synthetic guard for retrieval and admission regressions; qmd-modes asks one
# corpus the same questions through the built-in lexical adapter and through the
# qmd adapter with one retrieval layer added at a time, so a metric that moves is
# attributable to the layer that moved it. Run artifacts carry excerpts, so they
# go to a temporary directory and never into the tree.
#
# build-all rather than build: the core spawns external adapters by command name,
# and qmd-modes spawns recall-qmd. Its sources replay recorded qmd output, so the
# pack needs neither a network nor qmd installed.
EVAL_PACKS := smoke shapes qmd-modes

.PHONY: eval
eval: build-all
	@d=$$(mktemp -d) && trap 'rm -rf "$$d"' EXIT && \
	for p in $(EVAL_PACKS); do \
		echo "== $$p"; \
		PATH="$(PWD)/$(BIN):$$PATH" $(BIN)/recall eval run \
			--pack eval/packs/$$p --output "$$d/$$p" || exit 1; \
		$(BIN)/recall eval compare eval/baselines/$$p.json "$$d/$$p" || exit 1; \
	done

# This is both a local release dry run and the parity guard between cmd/ and
# published archives. The verifier derives its expected binaries from cmd/, so
# adding a command without adding it to GoReleaser fails here.
release-snapshot:
	$(GORELEASER) check
	$(GORELEASER) release --snapshot --clean
	./scripts/verify-release-archives.sh dist
	./scripts/test-release-guards.sh dist
	./scripts/test-release-publication.sh
	@d=$$(mktemp -d) && trap 'rm -rf "$$d"' EXIT && \
		./scripts/render-homebrew-formula.sh v0.1.0 \
			0000000000000000000000000000000000000000000000000000000000000000 \
			"$$d/recall.rb"

# Fail closed before creating a release tag. In particular, compare HEAD with
# the live remote rather than a possibly stale origin/main tracking ref.
check-release-state:
	@test -n "$${RELEASE_VERSION:-}" || { echo 'RELEASE_VERSION=vX.Y.Z is required' >&2; exit 2; }
	./scripts/check-release-state.sh pre-tag

.NOTPARALLEL: release-preflight release release-publish

release-preflight: check-release-state
	$(MAKE) check
	$(MAKE) eval
	$(GO) vet ./...
	git diff --check
	$(MAKE) release-snapshot

# Cut a release: derive/stamp the version, push main, then run the preflight
# and publish. Write bullets under `## [Unreleased]` in CHANGELOG.md, then
# either let this derive the version (BUMP=major|minor|patch) or set
# RELEASE_VERSION=vX.Y.Z yourself.
release:
	./scripts/release.sh

# Print the release plan (derived version, changelog stamp, commit, push,
# publish) and stop before any mutation.
release-dry-run:
	./scripts/release.sh --dry-run

# main must already be published and CI must already be green. This target
# creates the tag, waits for its exact release workflow, then publishes and
# remotely verifies the Homebrew formula using the local operator's explicit
# authorization for the tap repository. `release` calls it after pushing; call
# it directly with RELEASE_VERSION=vX.Y.Z to resume a release whose changelog
# commit is already on main.
release-publish: release-preflight
	./scripts/publish-release.sh

# Idempotent recovery when the tag exists but its workflow or tap publication
# failed. It re-verifies the exact tag, workflow, public archive, and remote tap.
release-tap:
	@test -n "$${RELEASE_VERSION:-}" || { echo 'RELEASE_VERSION=vX.Y.Z is required' >&2; exit 2; }
	./scripts/publish-homebrew-tap.sh

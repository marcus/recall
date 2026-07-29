GO      ?= go
PKG     := github.com/marcus/recall
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X $(PKG)/pkg/buildinfo.Version=$(VERSION) \
           -X $(PKG)/pkg/buildinfo.Commit=$(COMMIT)
BIN     := bin

PREFIX  ?= $(HOME)/.local

.PHONY: all build build-all install uninstall test lint fmt cover clean tidy check

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

lint:
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
	rm -rf $(BIN) coverage.out

# eval runs the committed packs through the same application layer the CLI
# uses, and compares each against its committed baseline. It is a separate
# target from `check` because it builds several binaries and spawns them; CI
# runs both.
#
# Both packs run, and a failure in either fails the target. smoke covers the
# failure vocabulary a healthy corpus cannot produce; shapes is a compact
# synthetic guard for retrieval and admission regressions. Run artifacts carry
# excerpts, so they go to a temporary directory and never into the tree.
#
# build-all rather than build: the core spawns external adapters by command
# name. The packs currently use built-ins only, but build-all keeps this target
# honest if a future synthetic fixture adds an external adapter.
EVAL_PACKS := smoke shapes

.PHONY: eval
eval: build-all
	@d=$$(mktemp -d) && trap 'rm -rf "$$d"' EXIT && \
	for p in $(EVAL_PACKS); do \
		echo "== $$p"; \
		PATH="$(PWD)/$(BIN):$$PATH" $(BIN)/recall eval run \
			--pack eval/packs/$$p --output "$$d/$$p" || exit 1; \
		$(BIN)/recall eval compare eval/baselines/$$p.json "$$d/$$p" || exit 1; \
	done

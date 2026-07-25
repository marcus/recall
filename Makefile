GO      ?= go
PKG     := github.com/marcus/recall
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X $(PKG)/internal/buildinfo.Version=$(VERSION) \
           -X $(PKG)/internal/buildinfo.Commit=$(COMMIT)
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

# Adapter binaries must land on the same PATH as the CLI: the core spawns them
# by command name, so installing recall alone yields a config that passes
# validation and then fails to reach half its sources.
install: build-all
	@mkdir -p '$(PREFIX)/bin'
	@for f in $(BIN)/*; do \
		[ -f "$$f" ] && [ -x "$$f" ] || continue; \
		install -m 0755 "$$f" '$(PREFIX)/bin/' && echo "installed $$(basename $$f) -> $(PREFIX)/bin"; \
	done

uninstall:
	@for f in $(BIN)/*; do \
		[ -f "$$f" ] || continue; \
		rm -f '$(PREFIX)/bin/'"$$(basename $$f)" && echo "removed $$(basename $$f)"; \
	done

clean:
	rm -rf $(BIN) coverage.out

# eval runs the committed smoke pack through the same application layer the CLI
# uses. It is a separate target from `check` because it builds two binaries and
# spawns one; CI runs both.
.PHONY: eval
eval: build
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/recall-stream ./cmd/recall-stream
	PATH="$(PWD)/$(BIN):$$PATH" $(BIN)/recall eval run --pack eval/packs/smoke

GO      ?= go
PKG     := github.com/marcus/recall
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X $(PKG)/internal/buildinfo.Version=$(VERSION) \
           -X $(PKG)/internal/buildinfo.Commit=$(COMMIT)
BIN     := bin

.PHONY: all build test lint fmt cover clean tidy check

all: check

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/recall ./cmd/recall

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

clean:
	rm -rf $(BIN) coverage.out

# eval runs the committed smoke pack through the same application layer the CLI
# uses. It is a separate target from `check` because it builds two binaries and
# spawns one; CI runs both.
.PHONY: eval
eval: build
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/recall-stream ./cmd/recall-stream
	PATH="$(PWD)/$(BIN):$$PATH" $(BIN)/recall eval run --pack eval/packs/smoke

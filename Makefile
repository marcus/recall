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

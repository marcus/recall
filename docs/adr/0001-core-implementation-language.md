# ADR-0001: Core Implementation Language

## Status

Proposed

## Context

Recall will be a configurable local retrieval engine used by unrelated
projects. It needs a CLI, parallel source queries, process supervision for
external adapters, and reliable access to files, JSONL, and SQLite. The same
application core should support an MCP server and a long-lived local API when
those surfaces have consumers.

The core should be easy to install on macOS and Linux and reasonably portable
to Windows. Source adapters must not be restricted to the core language. Python
is excluded from the initial choice by owner preference.

## Decision Drivers

- One small operational footprint for a CLI and local daemon
- Safe concurrent I/O with cancellation and timeouts
- Good support for subprocesses, HTTP, JSON, SQLite, and filesystem watching
- An official, maintained MCP SDK
- Fast builds and straightforward tests
- Cross-platform release binaries
- A clean boundary for adapters written in other languages
- Enough performance for fusion and indexing metadata without premature
  systems-level optimization

## Considered Options

### Go

Go fits the application shape well. It produces native executables, has a strong
standard library for CLI and service work, and makes parallel adapter queries
and cancellation straightforward with goroutines and contexts. Its
`database/sql` ecosystem supports SQLite and other relational sources.

The [official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) is now
Tier 1 and implements both clients and servers. MCP no longer creates a strong
ecosystem reason to prefer TypeScript.

Go's native shared-library plugin mechanism is a poor fit. The
[standard library documentation](https://pkg.go.dev/plugin) warns about
platform limits, build-toolchain coupling, dependency version mismatches, and
difficult race detection. It recommends considering IPC. Recall needs an
out-of-process adapter protocol regardless of the core language, which removes
this drawback.

The main weakness is local machine-learning integration. Embedding and
reranking libraries appear first in Python, C++, Rust, or TypeScript. Recall can
call QMD or another model service behind an adapter instead of embedding that
runtime into the core.

### TypeScript on Node.js or Bun

TypeScript offers the quickest path to QMD library integration and has a broad
ecosystem for agent protocols and data adapters. The official MCP TypeScript SDK
is Tier 1 and runs on Node.js, Bun, and Deno. Bun can
[compile a standalone executable](https://bun.sh/docs/bundler/executables) for
common macOS, Linux, and Windows targets.

The tradeoff is a larger and more variable operational surface. Native modules,
model bindings, runtime differences, and package ecosystem churn complicate the
"install one dependable local tool" goal. TypeScript types also stop at runtime
boundaries, so configuration and adapter messages still require strict runtime
validation.

TypeScript becomes the better choice if direct QMD library integration or a
large ecosystem of in-process adapters matters more than a conservative core.

### Rust

Rust offers the strongest control over performance, memory use, and native
distribution. It would be attractive if Recall itself became a high-performance
indexing or vector-search engine.

That is not the current product boundary. Rust would slow early iteration and
adapter development without addressing a measured bottleneck. The official MCP
SDK list currently places Rust at Tier 2 while Go and TypeScript are Tier 1.

### Multiple In-Process Languages

Embedding another runtime in the core would make packaging, debugging, and
failure isolation worse. External adapters and sidecars give us the useful part
of a polyglot system with a clear process boundary.

## Proposed Decision

Use Go for the Recall core.

Build the CLI, configuration loader, source orchestrator, fusion engine,
evidence expansion, and evaluation runner in one Go module. Add MCP as a thin
transport next. Add the local HTTP API when a long-lived client or shared
process creates a concrete need. Keep QMD and other specialized engines behind
versioned external adapter boundaries.

Do not use Go shared-library plugins. Support built-in adapters plus
out-of-process adapters over a versioned request/response protocol.

This remains proposed until a short implementation spike proves:

1. One built-in source and one external source can be searched concurrently.
2. CLI and MCP return the same typed query result.
3. Cancellation terminates a slow external adapter.
4. A cross-platform build produces clean binaries without hidden runtime
   dependencies.
5. The benchmark runner can execute deterministic fixture cases.

## Consequences

The common installation can be one executable. The same application code can
serve CLI, HTTP, and MCP clients. Concurrency and process management stay in a
language suited to both.

QMD will run as a sidecar or subprocess instead of an imported library. That
adds a process boundary and serialization cost, but it improves failure
isolation and keeps the document backend replaceable.

Adapter authors can use any language. The adapter protocol and conformance
tests become public contracts that need versioning and compatibility rules.

## References

- [Official MCP SDK tiers](https://modelcontextprotocol.io/docs/sdk)
- [Official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
- [Go plugin warnings](https://pkg.go.dev/plugin)
- [Go database access](https://go.dev/doc/database/)
- [Bun standalone executables](https://bun.sh/docs/bundler/executables)

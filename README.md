# Recall

Recall is a portable retrieval layer for personal AI agents. It searches
heterogeneous, user-controlled sources and returns compact evidence with stable
provenance.

Each source answers a narrow question — "which of my records best match this
request?" — in its own native way. Recall decides which sources are eligible,
fuses their ranked lists without comparing incomparable scores, groups
projections of the same record into one evidence lineage, and returns pointers
the agent can expand under a budget.

- [Specification](docs/spec.md) — contracts, ranking, trust, time, surfaces
- [Adapter protocol](docs/adapter-protocol.md) — message shapes, transport,
  conformance
- [Writing an adapter](docs/writing-an-adapter.md) — the working guide, and
  [`templates/adapter-python`](templates/adapter-python) to copy
- [Evaluation](docs/evaluation.md) — packs, metrics, gates, research policy
- [Profile example](docs/profile-example.md) — a concrete source inventory
- [HTTP and MCP surfaces](docs/surfaces.md) — service mode, authentication,
  endpoint semantics, and agent-host tools

Status: the first vertical slice is in. The CLI fuses two heterogeneous sources
— indexed project documents and live structured tasks — plus a reference
external adapter that exercises the wire protocol and evidence lineage, and an
evaluation harness with a committed smoke pack.

External adapters ship here as separate commands:

- `recall-stream` — the reference implementation, and the smallest complete
  thing an adapter author can copy: an append-only JSONL event source that
  emits `derived_from` edges.
- `recall-ongoing` — the ongoing project catalog over its HTTP API: every local
  repository with its git, LOC, td, GitHub, and traffic measurements, and the
  attention classifications ongoing computes over them, carried through with
  the reasons behind each one. See `cmd/recall-ongoing/ongoing` for what it
  declares and why.

An adapter that lives outside this repository starts from
`templates/adapter-python`: a complete adapter in one dependency-free Python
file, shipping recorded conformance transcripts, so a copy inherits a passing
suite rather than an empty one. It is Python rather than Go on purpose — an
external adapter is a process speaking JSON-RPC on stdio, and a Go template
against this module's internal packages would prove the packages rather than the
wire and could not be copied out of the tree at all.

`eval/baselines/smoke.json` is the frozen smoke run. CI runs the pack and
`recall eval compare`s the result against it on every change, and the comparison
exits non-zero if any rate moves down, so drift fails a build instead of waiting
to be noticed. Refresh it deliberately when a change is meant to move a number —
see [evaluation.md](docs/evaluation.md#layout).

Quote a metric with the population it came from. Overall pools cases, the
case-tag macro weights tags equally, and they differ by construction: nDCG@10 is
0.7665 pooled and 0.7615 across tags on the same run.

## Install

```sh
make install            # builds every command into ~/.local/bin
make install PREFIX=/usr/local
```

`install` builds **all** binaries under `cmd/`, not just the CLI. External
adapters are separate executables that the core spawns by command name, so
installing `recall` alone produces a configuration that validates and then
cannot reach half its sources. `make uninstall` removes them again.

Verify:

```sh
recall version
recall doctor           # config validity, trust boundary, health, freshness
```

## Use

```sh
recall query "what did we decide about ranking"
recall query "…" --json
recall expand <locator> --detail full
recall sources          # instances, capabilities, health, freshness
recall config explain   # resolved configuration and where each value came from
recall eval run --pack eval/packs/smoke
recall serve            # long-lived local HTTP API, loopback by default
recall mcp              # MCP tools over stdio
```

Exit codes distinguish `answered` (0), `error` (1), `abstained` (2),
`degraded` (3), and `failed` (4) — "nothing matched" and "a source could not
answer" are never the same result.

Configuration lives at `~/.config/recall/config.toml` with adapter definitions
in `adapters.d/`; `recall config explain` prints the resolved view. Adapter
commands may only be declared in user-level configuration — see the
[trust boundary](docs/spec.md#layers-and-trust-boundary).

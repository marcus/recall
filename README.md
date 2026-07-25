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
- [Evaluation](docs/evaluation.md) — packs, metrics, gates, research policy
- [Profile example](docs/profile-example.md) — a concrete source inventory

Status: the first vertical slice is in. The CLI fuses two heterogeneous sources
— indexed project documents and live structured tasks — plus a reference
external adapter that exercises the wire protocol and evidence lineage, and an
evaluation harness with a committed smoke pack and baseline.

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
```

Exit codes distinguish `answered` (0), `error` (1), `abstained` (2),
`degraded` (3), and `failed` (4) — "nothing matched" and "a source could not
answer" are never the same result.

Configuration lives at `~/.config/recall/config.toml` with adapter definitions
in `adapters.d/`; `recall config explain` prints the resolved view. Adapter
commands may only be declared in user-level configuration — see the
[trust boundary](docs/spec.md#layers-and-trust-boundary).

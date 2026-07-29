# Recall (`github.com/marcus/recall`)

An agent working on your behalf needs to find decisions, tasks, and project
state scattered across tools that have no common index. `grep` can search one
tree; it cannot ask a notes directory, a task tracker, and a live project
catalog the same question, or tell you which of those sources failed to answer.
Recall searches each source on its own terms and returns one compact, honest
set of evidence pointers.

Here is the human output captured by Recall's deterministic CLI fixtures.
These are pointers, not copied documents: choose one, then pass its locator to
`recall expand`.

```text
$ recall query "backup-restore.md"
outcome answered  coverage complete  results 2

1. notes:runbooks/backup-restore.md#L3-L7  exact
   Backup And Restore > Restore Drill
 > Every quarter we restore the newest snapshot into a scratch volume and compare checksums against the source. The drill is scheduled by a task so its rotation is visible.

$ recall query "marcus"
outcome answered  coverage complete  results 1

1. docs:team.md  corroborated 2
   title team.md
   excerpt for team.md

$ recall query --profile smoke-degraded "checksum newest scratch volume"
outcome answered  coverage degraded  results 1
degraded coverage: flaky (partial), slow (timeout)

1. notes:runbooks/backup-restore.md#L3-L7
   Backup And Restore > Restore Drill
 > Every quarter we restore the newest snapshot into a scratch volume and compare checksums against the source. The drill is scheduled by a task so its rotation is visible.
```

Recall keeps the facts that matter when sources disagree or fail:

- It fuses ranked lists without pretending one source's score is comparable to
  another's.
- It follows declared lineage and groups multiple projections of one record as
  one piece of evidence.
- Exit codes distinguish a complete miss from an incomplete search. An
  abstention exits 2; degraded coverage exits 3.
- Records withheld by sensitivity, relevance, deduplication, or response budget
  are counted with their reason. They do not silently disappear.

## Quickstart

Start with the [quickstart](docs/quickstart.md). It covers installation,
`recall init`, indexing a directory you control, the first query, and expansion.
The [profile example](docs/profile-example.md) shows how to add more sources
once the first one works.

## Documentation

- [Specification](docs/spec.md) — contracts, ranking, trust, time, and surfaces
- [Adapter protocol](docs/adapter-protocol.md) — message shapes, transport, and
  conformance
- [Writing an adapter](docs/writing-an-adapter.md) — the working guide, with a
  dependency-free [Python adapter template](templates/adapter-python)
- [Gmail adapter](docs/gmail-adapter.md) — read-only gog setup, safe defaults,
  configuration, and coverage behavior
- [Evaluation](docs/evaluation.md) — packs, metrics, gates, and research policy
- [Profile example](docs/profile-example.md) — a concrete source inventory
- [HTTP and MCP surfaces](docs/surfaces.md) — service mode, authentication,
  endpoint semantics, and agent-host tools

## Install

Recall requires Go 1.26.4 or newer.

```sh
git clone https://github.com/marcus/recall.git
cd recall
make install            # builds every command into ~/.local/bin
make install PREFIX=/usr/local
```

`make install` builds every command shipped by this repository: the Recall
core, `recall-stream`, the reference external adapter, and two optional
first-party adapters, `recall-gmail` and `recall-qmd`. Neither has a
compile-time dependency on the tool it drives, and neither does anything until a
source is configured for it: `recall-gmail` needs
[`gog`](https://github.com/openclaw/gogcli) on `PATH`, and `recall-qmd` needs
[`qmd`](https://github.com/tobi/qmd) plus its local models — see
[qmd-adapter.md](docs/qmd-adapter.md), which also explains that qmd runs
*alongside* the built-in lexical document adapter rather than replacing it.
Private, consumer-specific adapters live with the consumers that configure them.
`make uninstall` removes the same command set.

Verify the installation and resolved trust boundary:

```sh
recall version
recall doctor
```

## Use

```sh
recall query "what did we decide about ranking"
recall query "…" --explain       # provenance, lineage, scores, source outcomes
recall query "…" --json          # compact pointers for a program
recall query "…" --json --explain
recall expand <locator> --detail full
recall refresh [--source <source-id>]
recall sources
recall config explain
recall eval run --pack eval/packs/smoke
recall serve                     # local HTTP API; loopback by default
recall mcp                       # MCP tools over stdio
```

Exit codes are part of the query contract:

| Code | Outcome | Meaning |
| ---: | --- | --- |
| 0 | answered | Results were returned and every eligible source answered. |
| 1 | error | Usage, configuration, or another command-level error. |
| 2 | abstained | Nothing matched and at least one source answered. |
| 3 | degraded | One or more eligible sources could not answer. |
| 4 | failed | Every source asked failed. |

The default output is deliberately small: rank, locator, title, excerpt, and
the claims needed to interpret them. `--explain` adds source-local scores,
provenance, lineage, source outcomes, and the resolved plan. `--json` changes
the encoding, not the information tier.

Configuration lives at `~/.config/recall/config.toml`, with adapter definitions
under `~/.config/recall/adapters.d/`. `recall config explain` prints the merged
configuration and the file that supplied each value. Adapter commands may only
come from user-level configuration; a repository's `recall.toml` cannot make
Recall execute a program.

## Go adapter SDK

Go adapters import `github.com/marcus/recall/pkg/adapter`,
`github.com/marcus/recall/pkg/protocol`, and
`github.com/marcus/recall/pkg/recall`; recorded transcript tests use
`github.com/marcus/recall/pkg/conformance`.

The SDK is pre-1.0: minor releases may include breaking API changes. Every
breaking SDK change will be listed in `CHANGELOG.md` with migration guidance.
See [Writing an adapter](docs/writing-an-adapter.md#go-adapters) for the
external Go adapter workflow.

## Evaluation and conformance

`make eval` runs the two committed packs through the same application layer as
the CLI, then compares them with `eval/baselines/smoke.json` and
`eval/baselines/shapes.json`. `smoke` covers the broad protocol and failure
vocabulary. `shapes` is wholly synthetic and network-free; it guards the
retrieval shapes that tend to regress, including query-anchored excerpts,
heading-only representatives, link-destination exclusion, duplicate-view
fusion, honest abstention, bounded natural-language breadth, and named
below-floor suppression. Real development packs stay outside the clone.

Any overall or group-specific rate that moves down fails comparison. When a
deliberate retrieval change moves a metric, re-record the affected baseline in
the same commit and explain the trade in the pack note; see
[evaluation.md](docs/evaluation.md#layout).

External adapters, including `recall-gmail` and `recall-qmd`, ship recorded
JSON-RPC transcripts. A transcript captures
what the real adapter process returned, with only declared volatile fields
masked. Replay it with:

```sh
recall doctor --conformance <adapter>
```

The format and required cases are documented in
[Writing an adapter](docs/writing-an-adapter.md#conformance).

## Project status and name

Recall is pre-1.0. The CLI, HTTP API, MCP tools, built-in document, Tasks, and
td sources, reference stream and first-party Gmail adapters, external adapter
protocol, conformance runner, and evaluation harness are working;
compatibility may still change before the first stable release.

The project will publish as Recall, and the module and repository path
`github.com/marcus/recall` are its canonical identity. The bare command name is
crowded. In particular,
[`hyperengineering/recall`](https://github.com/hyperengineering/recall) is an
active Go-installable and Homebrew-distributed AI-memory CLI with the same
binary name, and similarly named packages exist on PyPI and npm. Use the fully
qualified repository or module path when installing and linking to this
project. The name will be revisited before 1.0 if command-level confusion
becomes material.

## Contributing and license

See [CONTRIBUTING.md](CONTRIBUTING.md) before sending a change, especially for
adapter transcript and evaluation-baseline rules. Security reports belong in
the private channel described in [SECURITY.md](SECURITY.md).

Recall is licensed under the [Apache License 2.0](LICENSE).

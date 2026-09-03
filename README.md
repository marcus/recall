# Recall (`github.com/marcus/recall`)

Always check if you are running in Sidecar: run `sidecar agents` for capabilities.
Recall is also a Sidecar plugin; see [Sidecar plugin](#sidecar-plugin).

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

Recall requires Go 1.27.0 or newer.

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
recall sidecar-plugin            # one Sidecar plugin request on stdin/stdout
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
configuration and the file that supplied each value. In `[defaults]`,
`budget_ms` is the end-to-end request latency ceiling when a caller omits
`--budget-ms`; `timeout_ms` remains the per-source inheritance default only.
Adapter commands may only come from user-level configuration; a repository's
`recall.toml` cannot make Recall execute a program.

## Sidecar plugin

`recall sidecar-plugin` answers the Sidecar plugin protocol
(`sidecar.plugin/v1-draft`): one JSON request on standard input, one JSON
response on standard output, one process per call. It is not meant to be typed.
Sidecar runs it from a `plugins.external` entry in
`~/.config/sidecar/config.json`:

```json
{
  "features": {"flags": {"plugin_protocol": true}},
  "plugins": {
    "external": [
      {
        "id": "recall",
        "command": ["recall", "sidecar-plugin"],
        "enabled": true,
        "placements": ["tab", "panes"]
      }
    ]
  }
}
```

No `passEnv` is needed for an ordinary install: `XDG_CONFIG_HOME`,
`XDG_STATE_HOME`, `XDG_CACHE_HOME`, and `HOME` are in the environment Sidecar
passes every plugin, which is everything recall reads to find its own
configuration and indexes. An adapter that needs a credential variable names it
in `passEnv`.

What it exposes:

| Surface | Contents |
| --- | --- |
| `results` collection | `recall query` through the configured profile. Search is *required*, because recall does not list, it answers. Columns are rank, title, source, and excerpt; the pill on a row is `exact` or `corroborated N`. Sortable by relevance, source, or updated. |
| `sources` collection | `recall sources`: one row per configured source with its health and the last instant it is known to have been complete. Polled every 120 s while it is on screen. |
| `results` document | `recall expand --detail full` on the row's locator, with an Evidence section and a Provenance section carrying the path and revision the text came from. |
| `sources` document | The source's configuration, health, freshness evidence, and declared capabilities. |
| `refresh-source` action | `recall refresh --source <id>` on a source row. It mutates, so Sidecar confirms it first. |
| `results` filters | Four choosers, read from configuration and applied per call: **profile** (every configured profile, defaulting to the configured default — this is the collection's scope, and its name is what Sidecar's pill shows), **source** (`Any` plus every configured source), **type** (`Any` plus the record types configuration declares), and **since** (an RFC 3339 date). A source outside the chosen profile is refused by name, with a profile that has it. |
| `project` context | Read to refuse a surface bound to another machine, and for nothing else. Recall answers globally: it applies no project narrowing of its own, because a project name is not something a documents source can evaluate. |

Recall declares no terminal matchers. Its locator is display-form
`<source_id>:<local>`, where the source name is user configuration and the local
part is adapter-owned, so any pattern wide enough to match it would also match
every URL scheme and every `key: value` pair printed in a terminal.

A page reports what the query claims, not just what it found: `answered`,
`abstained` (nothing matched and every eligible source answered), `degraded` (a
source that should have answered did not), or `failed` (every source asked
failed, so an empty list would claim nothing). The outcome describes the rows of
that page and nothing else — the `sources` collection answers `answered` even
when a source it lists is unwell, because the list is complete and each row
carries its own health.

What the page could not show travels with it as data rather than only as prose:
`omitted` counts the records suppressed below the relevance floor and the
results a response budget dropped, and `coverage[]` is one row per source with
its state (`answered`, `timeout`, `unhealthy`, `skipped`, `failed`), the reason
recall gave, and how long it took. The one-line notices remain the summary.

Check the wiring without opening the TUI:

```sh
sidecar plugin check recall --json
sidecar plugin check recall --list results --query ranking --json
sidecar plugin check recall --get results 'docs:architecture.md#L1-L8' --json
sidecar plugin call recall act \
  --params '{"action":"refresh-source","collection":"sources","id":"docs"}' --json
```

Both a typed success and a typed failure exit 0, because either one is an
answer. A non-zero exit means the response itself could not be written, and
Sidecar reads that as a crashed plugin.

A source that crashes is one of those typed failures rather than a crashed
plugin. Recall recovers a panic at two boundaries: the request itself, which
answers with the `internal` error code, and every per-source goroutine it fans
out onto for planning, query, and refresh, where a crash becomes that one
source's `panicked` failure while the other sources still answer. The crash
detail and its stack go to standard error, never to the single JSON object on
stdout. The guarantee stops at that per-source boundary: an adapter that starts
goroutines of its own is responsible for recovering on them, because that is a
stack recall never sees.

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

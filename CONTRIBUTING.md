# Contributing to Recall

Recall sits between an agent and private source material. A change that returns
more results can still be wrong if it hides a failed source, crosses a
sensitivity ceiling, invents lineage, or makes a locator impossible to expand.
Tests and fixtures in this repository treat those behaviors as contracts.

## Before you start

Open an issue before a large change so the interface and source boundary can be
agreed on first. Small fixes can go straight to a pull request.

The repository requires Go 1.26.4 or newer and `golangci-lint` 2.12.2. Run the
same gates CI runs:

```sh
make check
make eval
```

`make check` builds the core CLI, runs the race-enabled Go test suite, and runs
the linter. `make eval` builds every command, runs the committed evaluation
packs, and compares each result with its committed baseline.

Do not add personal corpora, credentials, machine-specific absolute paths, or
private-source excerpts to a fixture. Evaluation run artifacts contain
retrieved text and must stay outside the repository.

## Private `td-xxxxxx` references

You will see identifiers such as `td-ff5426` in comments, commit messages,
conformance notes, and evaluation-pack notes. They refer to the maintainer's
private task tracker. Outside contributors cannot resolve them, and no
contribution should depend on doing so.

The surrounding comment or document must carry enough context to explain the
decision on its own. If a private task reference is the only explanation you
can find, call that out in the issue or pull request and ask for the missing
context. New public contributions should use the GitHub issue or pull-request
number for public discussion.

## Adapter changes and conformance transcripts

An external adapter is a process that speaks newline-delimited JSON-RPC over
stdio. Its conformance suite is a set of recorded conversations with the real
process:

```text
conformance/
  <case>/manifest.json
  <case>/request.jsonl
  <case>/response.jsonl
  <case>/fixture/
```

If you change an adapter's protocol behavior:

1. Change the implementation and fixture.
2. Re-record the affected transcripts by running the adapter's recorder. For
   the reference Go adapter that is:

   ```sh
   go test ./cmd/recall-stream -run TestConformance -record
   ```

   The Python template uses `python3 conformance.py record`.
3. Read every transcript diff. A recorder observes current behavior; it does
   not decide whether that behavior is correct.
4. Replay the suite from a clean checkout:

   ```sh
   recall doctor --conformance <adapter>
   ```

Never hand-edit a response transcript. Only truly volatile values such as
timestamps or path-derived store identities belong in a manifest's `volatile`
list. Machine paths and secrets must not appear in a recorded response, even
when a volatile mask would make the replay pass. The complete format and
required cases are in
[Writing an adapter](docs/writing-an-adapter.md#conformance).

## Evaluation baselines

`make eval` compares each pack with a committed baseline. Any decrease in an
overall rate, a tag group, or a source-family group fails the comparison.

Most changes should leave the baseline untouched. If a deliberate ranking or
admission change moves a metric:

1. Run the affected pack into a temporary directory.
2. Inspect `summary.md` and `cases.jsonl`, including every result that moved.
3. Copy only `run.json` to the matching file under `eval/baselines/`.
4. Update the pack note with the measured trade and why it is acceptable.
5. Commit the implementation, pack changes, and new baseline together.

Do not update a baseline in a later cleanup commit, and do not regenerate one
merely to make CI green. The exact commands and artifact boundaries are in
[Evaluation](docs/evaluation.md#layout).

## Pull requests

Keep core behavior in the library and expose it through every applicable
surface. A feature available only in the UI or only in human-readable output
is incomplete; the CLI and structured output are first-class interfaces.

A pull request should include:

- the problem and the source/trust boundary it affects;
- tests for normal, empty, degraded, and denied behavior where applicable;
- CLI/API/MCP parity for a new capability;
- transcript or baseline diffs when one of those contracts changed;
- the commands you ran and their results.

Run `gofmt` on Go changes. Keep commits narrow enough that a reviewer can tell
which behavior each commit introduces.

## License

By submitting a contribution, you agree that it is licensed under the
[Apache License 2.0](LICENSE), as described by section 5 of that license.
Recall uses one repository-level license and does not add license headers to
individual source files.

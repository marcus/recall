# Conformance transcript format

This directory is the recorded conformance suite for `cmd/recall-stream`, in
the layout [docs/adapter-protocol.md](../../../docs/adapter-protocol.md#conformance)
requires. It is also the format specification the replay harness behind
`recall doctor --conformance <adapter>` consumes, because the protocol document
fixes the file names and nothing else: it does not say how a transcript
recorded on one machine is replayed on another, or how "declared-volatile
fields" are declared. This file settles both.

Nothing here is specific to this adapter. Any adapter shipping transcripts in
this shape can be replayed by the same harness.

## Layout

```text
conformance/
  <case>/manifest.json    what the case is, and how to replay it
  <case>/request.jsonl    one JSON-RPC message per line, order-significant
  <case>/response.jsonl   the responses that run produced, order-significant
  <case>/fixture/         source data the case is configured against
```

`response.jsonl` is recorded, never written by hand. For this adapter:

```sh
go test ./cmd/recall-stream -run TestConformance -record
```

The recorder is the replayer: `cmd/recall-stream/conformance_test.go` builds
the binary, drives each case against a real process, and either writes
`response.jsonl` or diffs against it. A transcript that was hand-written would
be a claim about the adapter rather than an observation of it.

## manifest.json

```json
{
  "case": "search-partial",
  "description": "Prose. What this case proves, and why the response is right.",
  "flow": "lockstep",
  "placeholders": {
    "FIXTURE": "absolute path of this case's fixture/ directory",
    "WORKDIR": "absolute path of a fresh, writable, empty directory"
  },
  "volatile": ["/result/checked_at", "/result/candidates/*/confirmed_at"],
  "responses": 3
}
```

| Field | Meaning |
| --- | --- |
| `case` | Must equal the directory name. A renamed directory with a stale manifest is a defect. |
| `description` | Why the case exists. A transcript nobody can read is not documentation; the replayer refuses an empty one. |
| `flow` | How lines are dispatched. `lockstep` is the only defined value; see below. |
| `placeholders` | Tokens substituted into every request line before sending, with what each must be bound to. |
| `volatile` | Paths whose values are ignored when diffing. |
| `responses` | How many frames the adapter is expected to write. Declared so a case that silently stops answering fails instead of passing with a short list. |

## Placeholders

`request.jsonl` cannot contain machine paths: a handshake carries an absolute
`workdir` and an absolute `location`, and both differ on every machine and
every run. Requests therefore carry `${NAME}` tokens, which the replayer
replaces by literal text before parsing the line.

Two are defined here, and a harness must bind both:

- `${FIXTURE}` — the absolute path of the case's `fixture/` directory. It is
  read-only as far as the case is concerned; a case that needs to mutate its
  source data should copy it first.
- `${WORKDIR}` — the absolute path of a fresh, writable, empty directory,
  distinct per case and per run. An adapter's index and checkpoints go here,
  and reusing one between runs would make a second replay observe a warm index.

Substitution is textual and happens before JSON parsing, so a path containing
characters JSON escapes must be escaped by the harness when it substitutes.

There is no `${NOW}`. Every request instead carries a fixed far-future
`deadline`, so a transcript replays identically today and in ten years without
the harness rewriting request fields. A case that needs an already-elapsed
deadline states one in the past explicitly.

## Flow: `lockstep`

Lines are sent in file order, and:

- After sending a **request**, wait for its response before sending the next
  **request**.
- Send a **notification** — a frame with a `method` and no `id` — immediately,
  without waiting for anything.

The second rule is what makes cancellation recordable: `recall/cancel` has to
reach the adapter while the request it abandons is still running, so a harness
that waited for every response before sending the next line would deadlock on
that case and record nothing.

After the last expected response, the harness closes the adapter's stdin and
drains stdout. Anything read during the drain counts toward `responses`, so an
adapter that writes an extra frame after a clean shutdown fails.

## Volatile fields

A `volatile` entry is a path from the root of one response **frame** — so it
begins `/result/…` or `/error/…`, not at the payload. The syntax is RFC 6901
JSON Pointer with one addition: the segment `*` matches every element of an
array and every member of an object. `/result/candidates/*/confirmed_at`
therefore covers a whole candidate list with one declaration.

The replayer replaces each matched value, in both the recorded and the live
frame, with a constant before comparing. A path that matches nothing is not an
error: cases share one declaration list across responses of different shapes.

Declare a field volatile only when it genuinely cannot repeat — wall-clock
timestamps, latencies, durations, process identity. Everything else is the
contract under test. In this suite that means `checked_at`, `last_success_at`,
`confirmed_at`, and `elapsed_ms`; candidate `observed_at` is **not** volatile,
because the fixture states it and an adapter that invented one instead should
fail.

## What a case may assume

- The adapter binary is started fresh per case, with no arguments.
- stdin receives the request lines; stdout carries protocol frames only.
- stderr is free-form and is never parsed. A harness should capture it and
  attach it to a failure report.
- The process is expected to exit on its own after `recall/shutdown` or after
  stdin closes. A harness must still kill it if it does not.

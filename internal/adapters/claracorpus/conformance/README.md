# recall-clara-corpus conformance suite

Recorded transcripts in the layout
[docs/adapter-protocol.md](../../../docs/adapter-protocol.md#conformance)
requires. The format itself — manifests, placeholders, the lockstep flow,
volatile fields — is specified once in
[cmd/recall-stream/conformance/FORMAT.md](../../../cmd/recall-stream/conformance/FORMAT.md)
and is not adapter-specific. Four things here are.

## Every record in every fixture is invented

A conformance suite is committed and the owner's real memory is not. That is the
same judgment that put the memory store's sensitivity floor at `confidential`,
applied to this directory: the fixtures are a plausible corpus about studio
insurance, written for the purpose, and none of it is anybody's actual life.

The fixtures are otherwise faithful. Field names, key order, schema versions,
ref grammar, lifecycle states, and the `observations-v1` provenance block are
all Clara's, taken from `~/code/clara/docs/data-contracts.md`.

## A case serves one store

An instance serves either `signals` or `memory`, so a case does too. The
required protocol cases run against the signal store; `search-memory` and
`expand-memory` are the two this source owes on its own, because the decay
arithmetic, the composite lineage of a generated preference, and the subject
history only exist there.

`expand-memory` also records the refusal that keeps them apart: a signals
locator handed to a memory instance is `locator_unknown`, not `locator_expired`,
because that record never lived there.

## `debug_today` pins the decay arithmetic

Every effective weight is a function of the day it was computed on, so a memory
transcript recorded today would drift a day at a time until it failed. The
memory cases pin the civil date to 2026-03-10 through `debug_today`, and health
reports both the date it aged to and the fact that it was pinned. It is a debug
key, declared in the settings schema, refused on a signals instance where
nothing ages anything, and it is what makes an evaluation pack over a memory
corpus reproducible as well.

Nothing else in a recording depends on a clock. Watermarks carry a deterministic
content digest alongside record/byte counts and the newest record date,
deliberately not a modification time: a checkout does not preserve one, and a
watermark that moved with the filesystem rather than with the data would say
nothing true.

The memory search fixture deliberately repeats one id. Its later line is the
candidate the search returns and the locator expands, holding the adapter's
deterministic repair-time last-write rule on the real protocol transport.

## `search-unavailable` has a directory where a file should be

The case has to record a store that exists and cannot be read, and the obvious
alternatives do not say that. A missing `signals.jsonl` means Clara has not
written the store yet, which is an empty store and complete coverage; a
permission bit is not preserved by a checkout. So the fixture holds a
*directory* named `signals.jsonl`, which opens and then fails to read, on every
platform, from a checkout, for ever. There is a README inside it saying so.

`store_identity` is masked in every case that probes health. Its value is an
absolute resolved path and is therefore the path of the machine that recorded
the transcript. Masking still holds the adapter to emitting the key: a live
frame missing it differs from a recorded frame that has it.

## Re-recording

```sh
go test ./internal/adapters/claracorpus -run TestConformance -record
```

`response.jsonl` is recorded, never written by hand: a transcript written by
hand would be a claim about the adapter rather than an observation of it. The
manifest's `responses` count is the contract a replay is held to, so a case
whose shape changed needs its manifest updated before it can be re-recorded.

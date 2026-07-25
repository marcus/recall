# recall-ongoing conformance suite

Recorded transcripts in the layout
[docs/adapter-protocol.md](../../../docs/adapter-protocol.md#conformance)
requires. The format itself — manifests, placeholders, the lockstep flow,
volatile fields — is specified once in
[cmd/recall-stream/conformance/FORMAT.md](../../recall-stream/conformance/FORMAT.md)
and is not adapter-specific. Two things here are.

## The fixture is a recorded HTTP conversation

This source is an HTTP API, so a case's `fixture/` holds recorded responses
rather than source data:

```text
fixture/<endpoint>.<status>.json
```

`<endpoint>` is the path under `/api` — `projects` or `health` — and
`<status>` is the HTTP status the instance answered with. The handshake points
the adapter at the directory through `settings.replay`, and no request leaves
the process.

Recording the status alongside the body is what lets a case cover the answers
that are not a good answer. `projects.401.json` is a denied instance;
`projects.503.json` would be one that cannot open its catalog; a directory with
no `projects.*` file at all is one that did not answer, which is what a refused
connection looks like to a replay. Without the status, only the happy path
would be recordable, and the invariant this adapter is most closely held to —
an unreachable source is never a successful empty result — would be the one
thing no transcript covered.

The replay transport reads no environment and never logs in. Authentication is
a property of a live instance; a case whose verdict depended on whether
`ONGOING_ACCESS_SECRET` happened to be exported would be a recording of the
machine rather than of the adapter. The secret path is covered by the Go tests
in `../ongoing`, against a real HTTP server.

## Every age in these recordings is fixed

The adapter measures the catalog's freshness from two timestamps the payload
carries — `generatedAt`, the server's clock when it built the response, and the
latest scan's `finishedAt` — and never from the clock of the machine reading
it. So `handshake` is a fresh catalog and `health-stale` is a six-day-old one,
permanently, and both replay to the same verdict in ten years.

## Re-recording

```sh
go test ./cmd/recall-ongoing -run TestConformance -record
```

`response.jsonl` is recorded, never written by hand: a transcript written by
hand would be a claim about the adapter rather than an observation of it. The
manifest's `responses` count is the contract a replay is held to, so a case
whose shape changed needs its manifest updated before it can be re-recorded.

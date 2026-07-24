# ADR-0003: External Adapter Wire Protocol

## Status

Proposed

## Context

Recall supports out-of-process adapters written in any language. The core
spawns and supervises them, so the protocol must cover a manifest handshake
with version negotiation, search, expansion, health, cancellation, and clean
shutdown. Adapters must be easy to write in a few dozen lines, easy to debug by
hand, and easy to conformance-test.

The spec fixes the message semantics; this ADR fixes the transport and framing.

## Decision Drivers

- An implementer can drive an adapter manually in a terminal and capture
  request/response transcripts as replayable conformance fixtures.
- Every plausible adapter language has a mature library for the chosen
  encoding, so adapter authors write handlers, not parsers.
- Cancellation and progress fit the protocol rather than bolting onto it.
- The same messages can later move to a network transport unchanged.
- No native shared-library plugins, per ADR-0001.

## Considered Options

### Newline-Delimited JSON-RPC 2.0 Over Stdio

One JSON-RPC message per line on stdin/stdout, stderr reserved for adapter
logging. This is the shape of MCP's stdio transport, so every ecosystem Recall
cares about already has hardened client and server libraries, and the pattern
is familiar to anyone who has written an MCP server. JSON-RPC contributes
request/response correlation ids, typed errors, and notifications for
cancellation and progress — exactly the fields a hand-rolled protocol
reinvents. Transcripts are line-oriented and diffable, which makes
record-and-replay conformance fixtures trivial.

The constraint is that messages cannot contain raw newlines and very large
payloads occupy one line. Recall's messages are compact by design — candidates
are pointers, and expansion is budget-bounded — so this costs little.

### LSP-Style Content-Length Framing

Header-plus-body framing handles arbitrary payloads and embedded newlines. It
solves a problem Recall's bounded messages do not have, and it makes manual
driving and transcript diffing meaningfully worse. Not chosen.

### gRPC Over A Local Socket (HashiCorp go-plugin Model)

Strong typing, streaming, and proven supervision patterns, but it front-loads
protobuf toolchains onto every adapter author and makes hand-driving and
transcript capture much harder. Wrong cost profile for adapters that should be
an afternoon's work. Not chosen.

### Plain MCP As The Adapter Protocol

Adapters would just be MCP servers. Tempting for reuse, but MCP tool results
are content-block shaped and weakly typed for Recall's candidate envelopes,
manifests, and health reports; the semantics would live in convention rather
than schema. Not chosen as the protocol — but the chosen transport is
MCP-shaped, so wrapping a real MCP server behind a small bridge adapter stays
cheap, and Recall's own MCP surface shares the same transport machinery.

## Proposed Decision

Use newline-delimited JSON-RPC 2.0 over stdio with Recall-typed methods:

```text
recall/initialize     handshake: protocol version range -> manifest,
                      negotiated version, capabilities
recall/search         search request -> candidates envelope
recall/expand         locator + budget -> evidence
recall/health         probe -> health report
recall/cancel         notification: abandon a request id
recall/shutdown       request clean exit
```

Rules:

- The core is the JSON-RPC client; the adapter is the server. stdout carries
  protocol frames only; stderr is free-form adapter logging.
- Version negotiation happens once in `recall/initialize`. An adapter that
  cannot satisfy the requested range fails the handshake explicitly.
- All requests carry deadlines. `recall/cancel` is advisory; the core enforces
  deadlines with SIGTERM, then SIGKILL, and reports the source outcome as
  failed or timed out, never as empty success.
- Message payloads are the spec's manifest, search, candidate, health, and
  expand contracts, validated by JSON Schema on both sides.
- Conformance is tested with recorded transcripts: a fixture adapter script
  and a directory of request/response line pairs that any adapter
  implementation must reproduce.

A future network transport reuses the same JSON-RPC messages over a socket or
HTTP without changing method semantics.

## Consequences

Adapter authorship in any language starts from an off-the-shelf JSON-RPC or
MCP-transport library plus five handlers. Debugging is `echo` and `jq`.
Conformance fixtures are text files in the repository.

The core owns process supervision: spawn, handshake, deadline enforcement,
kill, and restart policy. Large binary evidence would need chunking or a side
channel if it ever appears; the current contracts do not produce it.

## References

- [JSON-RPC 2.0 specification](https://www.jsonrpc.org/specification)
- [MCP stdio transport](https://modelcontextprotocol.io/docs/concepts/transports)
- [HashiCorp go-plugin](https://github.com/hashicorp/go-plugin)
- [ADR-0001: Core implementation language](0001-core-implementation-language.md)

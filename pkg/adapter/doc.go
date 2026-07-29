// Package adapter defines the single adapter contract and supervises the
// processes that implement it out of band.
//
// Boundary: one interface, two transports. A built-in adapter implements
// [Adapter] directly. An external adapter implements the same interface over
// JSON-RPC via pkg/protocol, reached through [Connect] for an existing
// stream or [External] for a subprocess Recall owns. [Serve] runs the bridge
// the other way, exposing any [Adapter] on a stream. Nothing in this package
// ranks, fuses, or interprets a locator; it moves requests to a source
// implementation and reports honestly about what came back.
//
// The core owns spawn, handshake, deadline enforcement, kill, restart policy,
// and pooling. Adapters own retrieval, ranking within their source, indexing,
// and locator semantics.
//
// Two rules are load-bearing enough to be worth stating here, because both are
// invariants rather than behaviors that could be tuned:
//
//   - A source that could not be reached, could not be spawned, could not
//     agree on a protocol version, or ran past its deadline never produces a
//     successful empty result. Every failure path goes through
//     [FailedSearch], which cannot return [recall.SearchSuccess].
//   - Deadline enforcement escalates: the advisory recall/cancel notification
//     first, then SIGTERM, then SIGKILL. An adapter that answers the cancel is
//     kept; one that does not is killed and respawned on next use.
package adapter

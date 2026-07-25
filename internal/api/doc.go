// Package api exposes the HTTP and MCP transports.
//
// Boundary: thin transports over the application core. A surface never acquires
// its own ranking, permission, or expansion behavior. Everything either surface
// answers comes from [Core]; nothing here decides what a source may be asked,
// what a result is worth, or how much evidence a caller may see.
//
// The two transports are written together and share [Core], the outcome
// mapping in outcome.go, and the request decoding in request.go. That is
// deliberate. They exist for the same reason — a caller that is not a terminal
// needs the same answers `recall query` gives a person — and the way one core
// grows two contradictory APIs is by two surfaces each interpreting the outcome
// vocabulary for themselves. Here the interpretation is written once and both
// surfaces read it.
//
// Two properties survive into both surfaces, because the system is built on
// them and a transport that flattened either would be lying:
//
//   - Outcome and coverage are independent and neither collapses. A caller can
//     always tell "nothing matched, and something looked" (abstained) from
//     "every source that was asked failed" (failed), and can always tell a
//     complete answer from one assembled out of an incomplete set of sources.
//     An empty result list is never on its own an answer.
//   - Locators round-trip. What a query returns is what an expansion accepts,
//     with no surface-local rewriting in between, so evidence a caller was
//     pointed at is evidence it can actually retrieve.
//
// HTTP binds loopback by default. A caller may explicitly select a non-loopback
// literal only when bearer authentication is configured; the bind guard refuses
// the unsafe combinations before a socket is opened. MCP speaks over the stdio
// of a process the user's own agent host spawned. Both call the configured
// profile's core, so neither can widen its sensitivity ceiling.
package api

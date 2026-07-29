// Package protocol implements the newline-delimited JSON-RPC 2.0 transport and
// message schemas of the Recall adapter protocol.
//
// Boundary: framing, correlation, error codes, and schema validation. It holds
// no retrieval or ranking logic and never interprets a locator, a rank, or a
// sensitivity level — it only checks that the shapes crossing the wire are the
// shapes the contract declares.
//
// The core is the client ([Client]); an adapter is the server ([Serve]). Both
// ends compile the same embedded schemas and validate in both directions, so a
// contract break is reported by whichever end introduced it rather than
// surfacing later as a confusing decode error.
//
// Three rules shape the code here:
//
//   - One message per line. A frame never contains a raw newline, so a
//     transcript is diffable and a bad frame resynchronizes at the next
//     newline instead of poisoning the stream.
//   - stdout carries frames only. An adapter's stderr goes to [Diagnostics],
//     which stores it and never parses it. Nothing written to stderr can
//     answer a request.
//   - A request that misses its deadline is a timeout, never an empty success.
//     [Client.Call] sends the advisory recall/cancel notification, waits a
//     bounded grace, and then reports [CallTimeout] with whether the adapter
//     answered — which is what tells a supervisor the process is wedged.
package protocol

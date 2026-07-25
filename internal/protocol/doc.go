// Package protocol implements the newline-delimited JSON-RPC 2.0 transport and
// message schemas of the Recall adapter protocol.
//
// Boundary: framing, correlation, error codes, and schema validation. It holds
// no retrieval or ranking logic and never interprets a locator.
package protocol

// Package adapter defines the single adapter contract and supervises the
// processes that implement it out of band.
//
// Boundary: built-in adapters implement [Adapter] directly; external adapters
// implement it over JSON-RPC via internal/protocol. There is one contract with
// two transports.
package adapter

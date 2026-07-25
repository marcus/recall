// Package stream is Recall's reference external adapter: an append-only JSONL
// event source, spoken over newline-delimited JSON-RPC 2.0 on stdio.
//
// It exists to be copied. Everything an adapter author has to decide once —
// how to negotiate a version, where an index may be written, what as_of
// support is honest, how a locator survives a source change, how cancellation
// is observed — is decided here in one small package, and every decision is
// recorded in cmd/recall-stream/conformance as a replayable transcript.
//
// # Boundary
//
// The adapter owns parsing, its projection, ranking within this source, and
// locator semantics. It owns no identity: source_uid, the source prior, and
// the sensitivity floor come from configuration, and the core overwrites the
// source part of every locator returned here. It writes only inside the
// workdir supplied at handshake, and only one file: cursor.json.
//
// # Lineage
//
// This is the only adapter in the tree that emits derived_from. A stream
// record that normalizes an upstream system's event carries that system's
// name and the upstream record's native ref; the settings block maps a system
// name to a configured source_id, and the pair becomes a display-form locator
// naming the upstream record. A signal about task td-f62256 therefore declares
// "tasks:td-f62256", which is character-for-character the locator the Tasks
// adapter writes for that task — so the two collapse into one lineage root and
// never corroborate each other. An unmapped system emits no edge at all:
// guessing a source_id would produce an edge that resolves somewhere, and a
// wrong lineage root is worse than a missing one.
//
// # Freshness and the checkpoint
//
// The projection is memory-resident and brought up to date incrementally by
// byte offset: an append-only file only ever grows, so catching up costs the
// bytes appended since the last pass and nothing more. A file that shrank was
// rewritten, which an append-only stream must not do, so the scan falls back
// to a full rebuild and health says so.
//
// cursor.json records the last successful boundary — generation, per-file
// offsets, record and failure counts. It is not a resume point for records:
// this adapter's index lives in memory and is rebuilt on start. It is what
// makes generations monotonic across restarts and what a fresh process
// compares against to notice that the stream was rewritten while it was down.
// An adapter with a durable index would write its records first and this file
// second, which is the ordering the spec requires and the reason it is written
// only after a generation is published.
//
// # as_of
//
// [recall.AsOfFilter], and deliberately not snapshot. Every record carries an
// immutable event_time, so restricting to events that happened at or before a
// boundary is a filter over history the source already stores — the thing the
// Tasks adapter could not do, because plan dates are not record history.
//
// Snapshot would be a lie. A record describing an early event can be appended
// at any later time, so the set of records present in the file at a past
// instant is not the set an event-time filter selects, and this format
// publishes no append time to reconstruct it from.
package stream

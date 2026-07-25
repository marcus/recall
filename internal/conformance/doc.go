// Package conformance replays recorded adapter transcripts.
//
// Boundary: one replay engine, two consumers. `recall doctor --conformance`
// drives an adapter binary through a suite and diffs what it says against what
// was recorded; the evaluation runner replays the same recordings in place of a
// live source so a model-backed or network-backed adapter produces reproducible
// benchmark runs. Both read the same transcripts through [Load], mask the same
// declared-volatile fields, and speak the same lockstep discipline. Nothing here
// ranks, fuses, or interprets a locator, and nothing here records: a transcript
// is evidence about an adapter, and the tool that writes one lives beside the
// adapter that produced it.
//
// The format is normative in docs/adapter-protocol.md#conformance, with
// cmd/recall-stream/conformance/FORMAT.md as the worked example. Three rules
// are load-bearing enough to restate, because each one is why a transcript is
// replayable at all rather than a behavior that could be tuned:
//
//   - ${FIXTURE} and ${WORKDIR} are substituted textually, before parsing.
//     Without them a transcript is bound to the absolute paths of the machine
//     that recorded it. Deadlines are never substituted: every recorded request
//     states a fixed far-future one, so the harness never rewrites a request
//     field.
//   - Under flow "lockstep" a request waits for its own response before the
//     next request is sent, and a notification is sent immediately. The second
//     half is what makes a cancellation case recordable: waiting for the
//     response to the search being cancelled would deadlock.
//   - The manifest's response count is enforced. An adapter that stops
//     answering fails on the count rather than passing on a short list that
//     happened to match as far as it went.
package conformance

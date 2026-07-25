// Package eval defines evaluation packs and computes retrieval metrics over
// their results.
//
// Boundary: this package describes and measures. It reads a pack — its
// manifest, its cases, and, separately, its judgments — checks them against
// the schemas in eval/schema, and turns one run's per-case outcomes into
// grouped metrics. It does not retrieve, rank, expand, or spawn anything, and
// it holds no evaluation-only ranking implementation: a run goes through the
// same application layer as the CLI, and hands the outcome back here as data.
//
// Two separations are load-bearing.
//
// Cases and judgments are read by separate calls and never merged into one
// type, so a runner can hand an agent the queries while withholding the
// answers. [LoadCases] does not open the judgment file, and nothing in the
// [Case] type carries an answer.
//
// A metric that is undefined for a case is not zero. A case with no forbidden
// evidence cannot score zero on Forbidden@5 without diluting the rate, and a
// case with nothing authoritative to find cannot score zero on MRR@10 without
// inventing a regression. Every per-case metric therefore reports whether it
// applies, and every average records how many observations it was taken over.
//
// Metrics are reported per case tag, per source family, and as an unweighted
// macro average across groups, so one large document source cannot hide a
// regression in exact lookups, temporal questions, or no-answer cases.
package eval

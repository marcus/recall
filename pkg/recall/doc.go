// Package recall holds the domain types every other package speaks.
//
// Boundary: types and their invariants only. No IO, no ranking policy, no
// storage knowledge, and no dependency on any other internal package. If a
// type here needs to import a sibling, the type is in the wrong place.
//
// Two pairs of names look similar and are deliberately distinct:
//
//   - [Outcome] is what Recall did (answered, abstained, failed).
//     [Coverage] is whether every eligible source was searched. A response can
//     abstain with complete coverage or answer with degraded coverage.
//   - [Coverage] describes one response; [IndexCoverage] describes how much of
//     a source an index represents.
package recall

// Package ranking groups candidates by evidence lineage, clusters lineage
// groups by entity, and fuses them into one ordered result set.
//
// Boundary: pure functions over recall types. No IO, no adapter knowledge, no
// storage schema. Raw source scores are never compared here or anywhere.
package ranking

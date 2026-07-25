// Package schema holds the evaluation pack, case, judgment, and run JSON
// Schemas.
//
// Boundary: this package contains no logic. The schemas are a wire contract
// that tools outside this repository read, so they live beside the packs they
// describe rather than inside internal/. This file exists only because
// `go:embed` cannot reach upwards out of its own directory; compilation,
// validation, and every Go type mirroring these schemas live in
// internal/eval.
package schema

import "embed"

// FS holds every evaluation JSON Schema, keyed by file name.
//
//go:embed *.schema.json
var FS embed.FS

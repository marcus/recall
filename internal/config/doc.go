// Package config loads, merges, and validates Recall configuration.
//
// Boundary: owns the user/project layer merge and the trust boundary that
// keeps adapter commands out of project files. Knows nothing about retrieval,
// ranking, or adapter internals.
package config

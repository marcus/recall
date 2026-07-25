// Package source resolves configured source instances and decides eligibility.
//
// Boundary: owns the registry of source instances and the hard eligibility
// rules (scope, permission, health, budget, as_of support, record types).
// It contains no intent router and no identifier routing.
package source

// Package evidence sanitizes retrieved content and shapes responses to budget.
//
// Boundary: the trust boundary for source-derived text and the deterministic
// budget-allocation policy. Every candidate crosses this package before it
// reaches a terminal, API, MCP client, or model.
//
// What a response costs is not decided here. Shaping spends a budget against a
// [Cost] the calling surface supplies, because only the surface that renders a
// response knows what rendering it costs; this package owns the order the
// budget is spent in, not the prices.
package evidence

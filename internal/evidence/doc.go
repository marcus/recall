// Package evidence sanitizes retrieved content and shapes responses to budget.
//
// Boundary: the trust boundary for source-derived text and the deterministic
// excerpt-allocation policy. Every candidate crosses this package before it
// reaches a terminal, API, MCP client, or model.
package evidence

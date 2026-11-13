# Bounded HTTP embedding retry cache

During explicit embedding maintenance, the HTTP adapter reuses qmd's existing
llm_cache for exact formatted inputs and the full immutable embedding contract.
It validates cached vector shape, values, normalization and identity. New HTTP
results are validated before being saved, before qmd inserts its chunk batch.
Queries always call the provider, so cached values cannot conceal unavailable
coverage. The original local inference path is unchanged.

qmd skips completed documents but regenerates every chunk of an interrupted
partial document. This cache avoids repeating recent completed inference during
that recovery. Upstream occasionally prunes llm_cache to 1,000 entries, so this
is bounded recent-chunk acceleration, not an infinite resumability guarantee.
Compatibility preflight checks getCachedResult/setCachedResult before index work.

Mocked HTTP tests and an opt-in real qmd2.8.3 controlled interruption proof pass,
including restart without repeating completed inference and corruption/model
mismatch rejection. Parent Robison independently reviewed and approved exact
commit a9e4614, including export guards and the bounded-cache documentation.
The approved change was merged, pushed and installed only after the baseline
Metal staging process finished. Installed package compatibility and real home
HTML/CLI/MCP proofs passed. td-849ab9 is closed by the parent reviewer.

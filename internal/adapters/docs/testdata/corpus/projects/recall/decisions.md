# Recall Decisions

A running log of decisions with their reasons.

## ADR-0003 Adapter Protocol

We decided on newline-delimited JSON-RPC over stdio for external adapters.
Debugging is echo and jq, and the same messages travel unchanged over a socket
later.

## ADR-0007 Deletion

A record proven deleted upstream is excluded from the next published
generation. Its old locator expands to locator_expired rather than to a nearby
record.

## ADR-0011 Priors

Source priors are configuration, bounded to a reviewed range, and every applied
prior appears in the explanation.

# Recall Architecture

Recall federates retrieval across sources it does not own.

## Ranking

Cross-source fusion uses rank, never raw scores. A raw score from one engine
means nothing next to a raw score from another, so the pool is ordered by
reciprocal rank and priors.

### Corroboration

Corroboration counts units. Two chunks of one document are one thing said once,
so they collapse on the record identity rather than counting twice.

## Indexing

An index is a rebuildable projection. The core owns none of them; every index
belongs to the adapter that built it.

The builder writes a whole generation and publishes it with a single rename, so
an interrupted build costs freshness and nothing else. Example of the layout:

```sh
# this heading-looking line is inside a fence and starts no section
ls index/gen-000003-abcdef123456/
```

## Open Questions

Whether semantic retrieval earns its latency is an evaluation question, not a
design preference.

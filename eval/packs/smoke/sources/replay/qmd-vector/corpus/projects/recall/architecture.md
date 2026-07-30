# Recall Architecture

Recall searches several sources at once and returns compact evidence with
stable provenance.

## Fusion

Results from different systems are merged by rank, never by raw score. Two
sources cannot agree on what a score of 4.2 means, so only the order each
source produced survives into the fused list.

## Lineage Grouping

Candidates are grouped by lineage root before display. The root is the
original record a candidate projects, after following the derivation edges an
adapter declared. Nothing is inferred from content.

## Expansion

Expansion is stateless. A locator printed yesterday expands today, or fails
explicitly with the reason the source changed.

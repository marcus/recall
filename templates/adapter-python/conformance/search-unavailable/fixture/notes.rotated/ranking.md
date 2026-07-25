id: note-ranking
title: Ranking fuses ranks, never scores
date: 2026-03-11T09:00:00Z
tags: ranking, fusion
aliases: td-7f7640

Cross-source fusion consumes local rank. A source's native score is diagnostic
only: two engines' scores are not on one scale, so comparing them would invent
an ordering neither source claimed.

## Exact identifiers
An exact match on a stable identifier is a partition, not a bonus. A candidate
promoted this way outranks every scored candidate, and an unbounded substring
match never carries the signal.

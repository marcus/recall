# Recall

Recall is a portable retrieval layer for personal AI agents. It searches
heterogeneous, user-controlled sources and returns compact evidence with stable
provenance.

Each source answers a narrow question — "which of my records best match this
request?" — in its own native way. Recall decides which sources are eligible,
fuses their ranked lists without comparing incomparable scores, groups
projections of the same record into one evidence lineage, and returns pointers
the agent can expand under a budget.

- [Specification](docs/spec.md) — contracts, ranking, trust, time, surfaces
- [Adapter protocol](docs/adapter-protocol.md) — message shapes, transport,
  conformance
- [Evaluation](docs/evaluation.md) — packs, metrics, gates, research policy
- [Profile example](docs/profile-example.md) — a concrete source inventory

Status: specification complete, implementation starting. The first vertical
slice fuses two heterogeneous sources — indexed project documents and live
structured tasks — plus a reference external adapter that exercises the wire
protocol and evidence lineage.

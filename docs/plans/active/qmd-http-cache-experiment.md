# Uninstalled HTTP embedding cache experiment

This branch checkpoints a tested experiment, not an approved product change.
Parent paused adoption while measuring managed Metal acceleration for the
initial home corpus build. It is not merged or installed.

The installed adapter remains the independently approved b7d20ac implementation.
Home staging's first 120-second CPU pass persisted 64 vectors. Inspecting qmd
showed completed documents are skipped, but every chunk of an interrupted
partial document is regenerated on retry. That motivated this experiment.

The patch uses qmd's existing llm_cache during embedding maintenance only,
keyed by the exact formatted input and model/dimensions/format/tokenizer
contract. It persists validated HTTP results before qmd inserts its chunk batch.
Queries never use the cache, preserving unavailable-provider coverage.

Focused mocked HTTP tests passed. An opt-in real qmd2.8.3 test interrupted a
large document after its first chunk batch and proved restart did not repeat
completed inference; race enabled, 5.67 seconds. Lint passed.

Gap: no independent review or decision to adopt. Parent is measuring whether a
600-second bounded managed-GPU build eliminates the practical pressure. Keep
this separate until a concrete remaining need justifies the cache. Do not
install or merge it merely because tests pass.

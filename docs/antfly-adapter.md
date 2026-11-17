# Antfly Lite adapter

`recall-antfly` is an optional local semantic document adapter for macOS arm64. Recall owns corpus selection, document extraction, citations, and coverage. Antfly Lite searches a local, embedded vector index. Rook generates EmbeddingGemma vectors through a loopback endpoint with local-only routing. The same Recall source serves CLI, HTTP, and MCP.

## Install

Use macOS arm64 with cgo. Install the pinned Antfly Lite v0.3.1 embedded release in a durable local directory and verify it using the [runtime checksums](../internal/semantic/antfly/README.md). Keep the unpacked release layout together. Then run `make install-antfly`. Configure `ANTFLY_LIBRARY` in the trusted user-level Recall adapter environment to the absolute path of `lib/libantfly.dylib`. Inject the Rook credential with Hush on aerie.

The source takes a local corpus location, the document adapter's `extensions`, exclusions, sibling preference, and size settings, plus:

- `inference_url` and `inference_model` for the local Rook endpoint and pinned model.
- `inference_dimensions` for the vector size (default 768).
- `inference_api_key_env` for the name of the optional credential environment variable.
- `cosine_floor` for admitting matches (default 0.30). Queries containing explicit numeric IDs such as `sku-104` also require a literal ID match in the document or path.

## Keep the index fresh

On aerie, the existing `com.marcus.recall-refresh` launch agent runs at local minutes 08, 23, 38, and 53. Its runner refreshes the `deep` profile, which includes `clara-home-semantic`. A manual refresh uses the same path:

```sh
recall refresh --profile deep --source clara-home-semantic --json
recall sources --profile deep --json
```

The adapter scans the current document boundary on refresh. When the boundary matches, it skips the build. On edits, it embeds new or changed files and reuses vectors for unchanged files. Each build stages a new Antfly database, drains its maintenance, closes it, and makes three exact queries after reopening it read-only. Publication advances the shared generation only after validation and a final corpus scan. Readers retain access to the published generation while a rebuild runs. Changed and deleted documents are withheld while their index is stale, and Recall reports incomplete coverage. The latest successful generation stays available if Rook, ingestion, or reopening fails. A retry uses the next scheduled slot, or run the manual command while diagnosing.

The scheduler records source failures in `~/Library/Logs/recall-refresh.log`. Check that log and the source's `index_generation`, `index_watermark`, `source_watermark`, and `coverage` when diagnosing freshness. Use `recall refresh --profile deep --source clara-home-semantic --full` to recompute all embeddings. Generations and cached vectors are private, rebuildable state under Recall's source workdir.

## Rollback on aerie

Restore just the previous `clara-home-semantic` source block from the saved aerie QMD configuration, preserving its source UID. The old `qmd-rook` adapter registration and index can be kept during rollout. Ensure the installed QMD bridge resolves the real package and run a scoped refresh, then verify query and expansion through `deep`. Both adapters use original-file line-range locators.

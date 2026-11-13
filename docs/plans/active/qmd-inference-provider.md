# Optional qmd inference provider

Goal: Recall remains standalone at work. Only sources explicitly configuring
HTTP inference contact a provider; no Rook discovery or CLI requirement.

The existing path keeps invoking the configured qmd executable unchanged.
The optional path uses a small feature-checked Node bridge to qmd >=2.8 <3 (tested 2.8.3)'s
store and inference seams. qmd retains corpus selection, indexing, chunking,
SQLite vectors, cosine search, and snippets. Recall retains coverage,
admission, locators, expansion, fusion, and CLI/MCP application behavior.

Configured vector mode uses qmd searchVec directly, without expansion. The
installed qmd vsearch actually expands queries, contrary to prior Recall
documentation. Hybrid/full with HTTP inference are refused until complete
remote expansion/reranking contracts exist. No baseline regeneration.

Provider contract: POST /v1/embeddings with immutable model, formatted string
array input, encoding_format=float; response model must match, indexed vectors
must be finite and have configured dimensions. The provider accepts qmd's
Gemma query/document prefixes unchanged and returns normalized embeddings.
Tokenizer endpoints preserve qmd token chunking: /v1/tokenize {model,content}
returns tokens; /v1/detokenize {model,tokens} returns content.

A dedicated, explicit qmd index is required. The contract pins immutable model,
dimensions, query/document formatting and tokenizer version. Existing vectors
without a matching pin are refused, never adopted or cleared. Only explicit
refresh may initialize an empty index. Original weights/index/config remain.

Task td-b80012. Parent Robison owns Rook service, contract coordination and
home activation. No paid inference, model downloads, full corpus reindex, or
production configuration edits before coordination. Acceptance: standalone
path unchanged, mocked HTTP vector refresh/query, refusal on provider failure
or model/shape/pin mismatch, structured CLI/MCP coverage, make check/eval,
one coordinated independent Sol-high review, land/install/push/cleanup.

Implemented opt-in config, HTTP provider and qmd bridge. Package compatibility
smoke checks API, formats, chunk constants and snippets before opening an index.
Pins are verified through a read-only SQLite handle before schema initialization.
Default tests use mocked inference. Existing external-model tests now explicitly
require RECALL_QMD_LIVE=1. qmd 2.8 preserves punctuation in filenames; derived
mapping accepts current physical paths and old normalized handles safely.

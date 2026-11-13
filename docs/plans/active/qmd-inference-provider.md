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

Independent Sol-high review of 6199a23 found one missing-field validation gap:
omitted dimensions defaulted to zero. Fixed by rejecting absent dimensions
before index creation and declaring all provider requirements in the manifest.
CLI/MCP mocked-service proof passed with identical locators and original evidence
in /Volumes/Swift/proofs/recall/2026-11-13-http-mock-surfaces. Live backend proof
awaits parent readiness. Endpoint config accepts a root or OpenAI /v1 base URL.

## Home transition, pending backend proof

Implementation is independently approved at b7d20ac, merged/pushed to main,
installed, and the feature branch/worktree removed. Installed compatibility
smoke passes qmd 2.8.3. No production configuration, original qmd index, model
file, or full-corpus embedding has been changed by this work.

Read-only SQLite census, 2026-11-13:

| Collection | Active documents | Unique content hashes | Active chunk references | Text characters |
| --- | ---: | ---: | ---: | ---: |
| clara-home | 690 | 666 | 2,524 | 4,321,131 |
| clara-home-html | 19 | 19 | 108 | 226,414 |

Chunk references include duplicate bodies and do not predict exact request
counts. The old index is about 67 MB and contains 14,059 vector rows including
historical content. The largest active Markdown document is 174,211 characters.
The current semantic source has a 30-second source timeout. Measure actual
small-corpus CPU throughput before estimating a full rebuild's duration.

Safe sequence:

1. Prove the installed provider through CLI and MCP with real Rook on a dedicated
   three-document corpus/index, including original evidence expansion. Inject
   authentication with Hush into an environment variable; never save its value.
2. Stage only the primary Markdown collection into an unused permanent provider
   index. Use a separate Recall configuration and state root, the existing qmd
   collection definition, identical source UID, the pinned model identity, and
   the real home's selection settings. Ensure qmd reads its existing global
   configuration while Recall's config is isolated. Omit derived HTML during
   staging so the current local source's shared receipt remains untouched.
3. Use an explicit bounded refresh, initially 120 seconds overall, and a matching
   per-invocation ceiling. Stop after each attempt and inspect read-only counts,
   completed coverage and runtime. qmd persists completed embedding batches and
   skips them on subsequent refreshes; no force/clear operation is used. A timed
   out build remains incomplete and can resume. Do not start a hidden long loop.
4. After the primary index is healthy and known evidence expands correctly,
   enable only the existing home semantic source's optional provider settings.
   Keep its UID, display ID, profile memberships, selection rules and collection
   identity. Work/default configurations remain standalone. A home-only adapter
   registration can use Hush to inject the bearer variable for ordinary CLI/MCP
   invocations without requiring users to export credentials manually.
5. Restore derived HTML at activation and run one explicit bounded refresh. The
   19 HTML documents then use the final production shared mirror/receipt path.
   Primary embeddings already completed during staging are reused. Coverage
   stays honest if this final pass needs another bounded attempt. Do not build
   a staged HTML collection pointing at a temporary mirror and then silently
   repoint it.
6. Query and expand through the home deep profile. Keep the old qmd index and all
   local model files intact until parent confirms the transition is complete.

Parent coordinates backend installation, the live proof, rebuild budget and home
activation. The corpus census is planning evidence, not permission for an
unbounded full-corpus refresh.

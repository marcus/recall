# Optional qmd HTTP inference

Recall remains standalone. Sources without inference settings use the original
qmd executable and local model/index behavior, with no Rook discovery or probe.
Explicit HTTP sources use qmd's store and inference seams, preserving its corpus
indexing, token chunking, SQLite vectors, cosine retrieval and snippets. Recall
retains coverage, admission, fusion, locators and original evidence expansion.

The HTTP path is vector search without query expansion. Hybrid/full are refused
until complete remote expansion/rerank contracts exist. Local qmd 2.8 vsearch
still includes its own expansion. Existing eval baselines are unchanged.

A separate explicit index pins immutable model identity, dimensions, formatting,
tokenizer version and qmd fingerprint. Read-only preflight refuses mismatched
pins, unpinned vectors, foreign identities or vector-table dimensions before any
qmd migration. Only refresh creates or pins an empty provider index. The bridge
supports qmd >=2.8 <3 with API, exact formatting, chunking and snippet checks;
tested with 2.8.3. `recall-qmd --check-inference-package` is the no-inference smoke
for package upgrades, including Stanley's weekly qmd check.

HTTP embeddings receive exact preformatted strings and must return indexed,
finite, normalized vectors with the configured immutable identity/dimensions.
Remote tokenize/detokenize use that same model with no extra prefixes/special
tokens. Rook metadata is explicit local-only/background admission; generic HTTP
providers receive no Rook metadata. Credentials remain environment values passed
as Bearer headers; URLs and credentials never define vector-space identity.

Home activation keeps source UID YN5BK4BDJG39090R and all profile memberships.
Only clara-home-semantic uses the home-only qmd-rook adapter registration. Its
absolute Hush command injects the key into recall-qmd, and its absolute mise Node
shim works under the scheduled recall-refresh-all PATH. The installed 0600 and
tracked 0644 config copies match. Default/work configurations and the original
standalone qmd registration remain unchanged. Original index and model files are
preserved.

The first bounded CPU staging pass saved 64 vectors in 120 seconds. A temporary
Rook-managed Metal placement then completed the primary-only stage in 209.45
seconds: 690 Markdown documents, 666 unique bodies, 2,509 vectors, no partial
bodies. It added 2,445 vectors at about 11.7 chunks/sec. The 19 derived HTML
documents were added only at activation, preserving the live local receipt while
Markdown staged. That final refresh took 11.58 seconds. The active source reports
healthy/complete coverage over 709 documents and 2,628 vectors across 685 bodies.
Normal placement/schedule restoration and the final CPU-cold proof belong to
parent Robison; this child ended inference before that restoration.

Validation: independent Sol-high review approved b7d20ac after fixing the only
finding, omitted dimensions defaulting to zero. Full race suite, lint, vet, and
smoke/shapes/qmd-modes eval packs passed. A real qmd2.8.3 journey with mocked HTTP
proved refresh/query/expand, provider failure, model mismatch, derived mapping and
controlled interrupted-document recovery. Real Rook three-document CLI/MCP proof
passed. Real home Markdown and HTML queries both answered with complete coverage,
identical CLI/MCP locators and identical original evidence, with caller key unset
and scheduled PATH. Proofs are under
/Volumes/Swift/proofs/recall/2026-11-13-rook-provider-live and
/Volumes/Swift/proofs/recall/2026-11-13-home-rook-activation.

Implementation tasks: td-b80012 and td-849ab9. Code a9e4614 is merged, installed,
and pushed; temporary implementation branches/worktrees are removed. The small
existing-qmd-cache recovery addition is documented in qmd-http-inference-cache.md.

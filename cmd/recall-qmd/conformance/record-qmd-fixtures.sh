#!/usr/bin/env bash
# Record the qmd output the conformance fixtures replay.
#
# The conformance suite has two recorded layers and they are captured by two
# different tools. This script captures the INNER one: the bytes the real qmd
# CLI writes for each invocation the adapter makes. The OUTER one — the JSON-RPC
# transcripts in response.jsonl — is captured from the adapter process by
#
#     go test ./cmd/recall-qmd -run TestConformance -record
#
# Run this one first when qmd's output changes, then re-record the transcripts,
# then read both diffs.
#
# Why record at all, rather than write the fixtures by hand: every rule this
# adapter enforces is a rule about real qmd output. The snippet header that a
# locator's line range is parsed out of, the `--explain` trace the per-result
# attribution is built from, the fact that an empty result is `[]` and a missing
# collection is prose on stdout with exit 0 — a hand-written fixture would be a
# claim about qmd, and the adapter would then be tested against that claim
# instead of against the tool.
#
# Two substitutions are applied to what qmd wrote, and only two:
#
#   * The staging corpus path becomes ${REPLAY_DIR}/corpus. `qmd collection
#     show` reports the directory a collection indexes and the adapter compares
#     it against the configured location on every operation, so the fixture has
#     to contain a path — and the recording machine's absolute path would both
#     land a home directory in the repository and fail the comparison on every
#     other machine. ReplayRunner substitutes the token back at read time, which
#     keeps the check real and the fixture portable.
#   * The staging index path becomes ${REPLAY_DIR}/corpus/.qmd/index.sqlite, for
#     the same reason. It is only ever hashed into a store identity, and a
#     replaying source publishes none.
#
# Usage: record-qmd-fixtures.sh [staging directory]
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
corpus_src=$here/../testdata/corpus
stage=${1:-$(mktemp -d)}
rec=$stage/recorded

command -v qmd >/dev/null || { echo "record-qmd-fixtures: qmd is not on PATH" >&2; exit 1; }

echo "== staging in $stage"
rm -rf "$stage/corpus" "$stage/other-corpus" "$rec"
mkdir -p "$rec" "$stage/corpus" "$stage/other-corpus"

# The corpus is committed under testdata/ and copied here, so the fixture the
# transcripts replay and the corpus a live test indexes are the same bytes.
# hydration.md is held back for one step: an index that holds a document no
# vector represents is the honest partial boundary, and producing it is the only
# way to record what a partial coverage report really looks like.
(cd "$corpus_src" && find . -type f -name '*.md' ! -name 'hydration.md' -print0 |
	while IFS= read -r -d '' f; do
		mkdir -p "$stage/corpus/$(dirname "$f")"
		cp "$f" "$stage/corpus/$f"
	done)

cd "$stage/corpus"
qmd init >/dev/null
qmd collection add . --name fixture --mask '**/*.md' >/dev/null
qmd update >/dev/null
qmd embed -c fixture >/dev/null

echo "== recording the partial boundary"
mkdir -p "$stage/corpus/notes"
cp "$corpus_src/notes/hydration.md" "$stage/corpus/notes/hydration.md"
qmd update >/dev/null
qmd status >"$rec/status-partial.txt" 2>/dev/null
qmd query --json --explain --no-rerank -n 10 -c fixture -- \
	"who can clean my teeth" >"$rec/search-hybrid-partial.json" 2>/dev/null

echo "== recording the complete boundary"
qmd update >"$rec/update.txt" 2>/dev/null
qmd embed -c fixture >"$rec/embed.txt" 2>/dev/null
qmd status >"$rec/status.txt" 2>/dev/null
qmd collection show fixture >"$rec/collection-show.txt" 2>/dev/null
qmd --version >"$rec/version.txt" 2>/dev/null

echo "== recording searches"
# One paraphrase query, asked of every mode. It is the whole case for this
# adapter existing: the words of the question appear nowhere in the document
# that answers it, so bm25 honestly returns [] and the embedding-backed modes
# find it. It is also the case for recomputing relevance rather than trusting
# qmd's score, because the reranked modes return three unrelated documents
# alongside the right one at scores a caller cannot tell apart.
qmd search --json -n 10 -c fixture -- "who can clean my teeth" >"$rec/search-empty.json" 2>/dev/null
qmd search --json -n 10 -c fixture -- \
	"dental hygienist accepting new patients" >"$rec/search-bm25.json" 2>/dev/null
qmd vsearch --json -n 10 -c fixture -- "who can clean my teeth" >"$rec/search-vector.json" 2>/dev/null
qmd query --json --explain -n 10 -c fixture -- "who can clean my teeth" >"$rec/search-full.json" 2>/dev/null
# stderr on this one: it is the progress renderer, and it is byte for byte what
# qmd writes to STDOUT on a cold start when it is downloading or loading a
# model. Recording it as a stdout fixture is how the broken-contract case is
# tested without evicting 2GB of models to reproduce it.
qmd query --json --explain --no-rerank -n 10 -c fixture -- \
	"who can clean my teeth" >"$rec/search-hybrid.json" 2>"$rec/progress.txt"
qmd query --json --explain --no-rerank -n 1 -c fixture -- \
	"recall adapter model warm up" >"$rec/warm.json" 2>/dev/null
# Vector mode is the one mode whose own score becomes Recall's relevance, so both
# ends of its admission floor are recorded: a paraphrase that clears it, and a
# noise query that does not clear qmd's cosine threshold and comes back as `[]`.
qmd vsearch --json -n 25 -c fixture -- "who can clean my teeth" >"$rec/vector-paraphrase.json" 2>/dev/null
qmd vsearch --json -n 25 -c fixture -- "kodachrome zxqv" >"$rec/vector-noise.json" 2>/dev/null

echo "== recording a collection that indexes somewhere else"
cd "$stage/other-corpus"
printf '# Another corpus\n\nA different tree entirely.\n' >README.md
qmd init >/dev/null
qmd collection add . --name fixture --mask '**/*.md' >/dev/null
qmd collection show fixture >"$rec/collection-show-mismatch.txt" 2>/dev/null

echo "== substituting machine paths"
python3 - "$rec" "$stage" <<'PY'
import os, sys
rec, stage = sys.argv[1], sys.argv[2]
subs = [
    (os.path.join(stage, "other-corpus"), "${REPLAY_DIR}/other-corpus"),
    (os.path.join(stage, "corpus"), "${REPLAY_DIR}/corpus"),
]
for name in sorted(os.listdir(rec)):
    path = os.path.join(rec, name)
    with open(path, encoding="utf-8") as fh:
        body = fh.read()
    for old, new in subs:
        body = body.replace(old, new)
    if stage in body:
        raise SystemExit(f"{name} still names the staging directory")
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(body)
PY

echo "== writing case fixtures"
cases_with() { # $1 = case, rest = recorded files
	local case=$1; shift
	local dir=$here/$case/fixture
	rm -rf "$dir"
	mkdir -p "$dir"
	(cd "$corpus_src" && find . -type f -print0 | while IFS= read -r -d '' f; do
		mkdir -p "$dir/corpus/$(dirname "$f")"
		cp "$f" "$dir/corpus/$f"
	done)
	# A stated clock, so a recorded transcript carries the fixture's time rather
	# than the recording machine's and needs no volatile timestamp masking.
	printf '{\n  "now": "2026-07-29T12:00:00Z"\n}\n' >"$dir/clock.json"
	local spec
	for spec in "$@"; do
		cp "$rec/${spec%%:*}" "$dir/${spec##*:}"
	done
}

# The rule maps below are the fixture's half of the contract: which recorded
# output answers which invocation. They match on argv content rather than on an
# exact argv so a change to the candidate cap does not silently stop matching
# and fall through to "no recorded response".
rules() {
	cat >"$here/$1/fixture/qmd.json"
}

base_rules() { # $1 = case, $2 = search stdout file, $3 = search exit code
	rules "$1" <<JSON
{
  "comment": "Recorded qmd 2.5.3 output. Regenerate with conformance/record-qmd-fixtures.sh; never edit by hand.",
  "invocations": [
    {"contains": ["--version"], "stdout": "version.txt"},
    {"contains": ["collection", "show"], "stdout": "collection-show.txt"},
    {"contains": ["status"], "stdout": "status.txt"},
    {"contains": ["query"], "stdout": "$2", "exit_code": $3},
    {"contains": ["search"], "stdout": "$2", "exit_code": $3},
    {"contains": ["vsearch"], "stdout": "$2", "exit_code": $3}
  ],
  "default": {"stderr": "no recorded response", "exit_code": 1}
}
JSON
}

cases_with handshake version.txt:version.txt collection-show.txt:collection-show.txt status.txt:status.txt
base_rules handshake search-hybrid.json 0
cp "$rec/search-hybrid.json" "$here/handshake/fixture/search-hybrid.json"

cases_with version-rejection version.txt:version.txt collection-show.txt:collection-show.txt status.txt:status.txt search-hybrid.json:search-hybrid.json
base_rules version-rejection search-hybrid.json 0

cases_with search-ranked version.txt:version.txt collection-show.txt:collection-show.txt status.txt:status.txt search-hybrid.json:search-hybrid.json
base_rules search-ranked search-hybrid.json 0

cases_with search-partial version.txt:version.txt collection-show.txt:collection-show.txt status-partial.txt:status.txt search-hybrid-partial.json:search-hybrid.json
base_rules search-partial search-hybrid.json 0

cases_with search-full version.txt:version.txt collection-show.txt:collection-show.txt status.txt:status.txt search-full.json:search-full.json
base_rules search-full search-full.json 0

cases_with search-bm25-abstains version.txt:version.txt collection-show.txt:collection-show.txt status.txt:status.txt search-empty.json:search-empty.json
base_rules search-bm25-abstains search-empty.json 0

cases_with search-vector-similarity version.txt:version.txt collection-show.txt:collection-show.txt status.txt:status.txt vector-paraphrase.json:vector-paraphrase.json
base_rules search-vector-similarity vector-paraphrase.json 0

cases_with search-vector-noise version.txt:version.txt collection-show.txt:collection-show.txt status.txt:status.txt vector-noise.json:vector-noise.json
base_rules search-vector-noise vector-noise.json 0

cases_with search-invalid-response version.txt:version.txt collection-show.txt:collection-show.txt status.txt:status.txt progress.txt:progress.txt
base_rules search-invalid-response progress.txt 0

cases_with search-unavailable version.txt:version.txt collection-show.txt:collection-show.txt status.txt:status.txt
rules search-unavailable <<'JSON'
{
  "comment": "A qmd whose index cannot be opened. Exit is non-zero and the reason is on stderr; the adapter reports unavailable and never an empty successful search.",
  "invocations": [
    {"contains": ["--version"], "stdout": "version.txt"},
    {"contains": ["collection", "show"], "stdout": "collection-show.txt"},
    {"contains": ["status"], "stdout": "status.txt"},
    {"contains": ["query"], "stderr": "SqliteError: unable to open database file\n", "exit_code": 1}
  ],
  "default": {"stderr": "no recorded response", "exit_code": 1}
}
JSON

cases_with collection-mismatch version.txt:version.txt collection-show-mismatch.txt:collection-show.txt status.txt:status.txt search-hybrid.json:search-hybrid.json
base_rules collection-mismatch search-hybrid.json 0

cases_with expand-details collection-show.txt:collection-show.txt status.txt:status.txt version.txt:version.txt
base_rules expand-details search-hybrid.json 0
cp "$rec/search-hybrid.json" "$here/expand-details/fixture/search-hybrid.json"

cases_with expand-expired collection-show.txt:collection-show.txt status.txt:status.txt version.txt:version.txt
base_rules expand-expired search-hybrid.json 0
cp "$rec/search-hybrid.json" "$here/expand-expired/fixture/search-hybrid.json"

cases_with cancel-inflight version.txt:version.txt collection-show.txt:collection-show.txt status.txt:status.txt search-hybrid.json:search-hybrid.json
rules cancel-inflight <<'JSON'
{
  "comment": "A recorded query that takes 30 seconds. The delay is what makes cancelling a request that is still in flight recordable at all: an instant fixture would answer before the notification arrived.",
  "invocations": [
    {"contains": ["--version"], "stdout": "version.txt"},
    {"contains": ["collection", "show"], "stdout": "collection-show.txt"},
    {"contains": ["status"], "stdout": "status.txt"},
    {"contains": ["query"], "stdout": "search-hybrid.json", "delay_ms": 30000}
  ],
  "default": {"stderr": "no recorded response", "exit_code": 1}
}
JSON

cases_with shutdown version.txt:version.txt collection-show.txt:collection-show.txt status.txt:status.txt
base_rules shutdown search-hybrid.json 0

cases_with refresh version.txt:version.txt collection-show.txt:collection-show.txt status.txt:status.txt update.txt:update.txt embed.txt:embed.txt warm.json:warm.json
rules refresh <<'JSON'
{
  "comment": "The maintenance sequence: reindex, embed, then one throwaway query that forces the models to load so that a search never pays for a cold start.",
  "invocations": [
    {"contains": ["--version"], "stdout": "version.txt"},
    {"contains": ["collection", "show"], "stdout": "collection-show.txt"},
    {"contains": ["status"], "stdout": "status.txt"},
    {"contains": ["update"], "stdout": "update.txt"},
    {"contains": ["embed"], "stdout": "embed.txt"},
    {"contains": ["query"], "stdout": "warm.json"}
  ],
  "default": {"stderr": "no recorded response", "exit_code": 1}
}
JSON

echo "== done; now re-record the transcripts:"
echo "   go test ./cmd/recall-qmd -run TestConformance -record"

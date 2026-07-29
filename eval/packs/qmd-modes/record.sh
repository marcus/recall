#!/usr/bin/env bash
# Record this pack's qmd output, one replay directory per mode.
#
# The pack exists to attribute a retrieval gain to a LAYER. qmd stacks LLM query
# expansion, RRF fusion over a full-text list and a vector list, and a
# cross-encoder rerank, and a result that improved under all three at once tells
# nobody which of them earned its place. Running the same queries against the
# same corpus under `bm25`, `vector`, `hybrid`, and `full` — plus the built-in
# lexical adapter as the outside baseline — is what makes the question answerable.
#
# It replays rather than spawns qmd for two reasons. The expansion layer is a
# language model, so a pack built on the live tool would measure something
# slightly different every run; and qmd needs about 2GB of models that a CI
# machine has no business downloading. `replay` is the settled spelling for that
# across adapters and is what the runner's network policy reads.
#
# Each replay directory ships its own copy of the corpus. A replay pack is
# self-contained by construction: the recorded `qmd collection show` names the
# directory the collection indexes through a ${REPLAY_DIR} token, the adapter
# verifies that against the configured location on every operation, and evidence
# is read from the file rather than through qmd. The five copies under sources/
# are byte-identical and are written by this script from sources/corpus.
#
# Usage: eval/packs/qmd-modes/record.sh [staging directory]
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
corpus_src=$here/sources/corpus
stage=${1:-$(mktemp -d)}

command -v qmd >/dev/null || { echo "record: qmd is not on PATH" >&2; exit 1; }

# The queries this pack asks. Each is recorded under every mode, and the replay
# rules match on the query text so one directory answers all of them.
queries=(
	"who can clean my teeth"
	"dental hygienist accepting new patients"
	"quantum chromodynamics lattice gauge"
)
slugs=(paraphrase lexical off-corpus)

echo "== indexing the pack corpus in $stage"
rm -rf "$stage/corpus"
mkdir -p "$stage/corpus"
(cd "$corpus_src" && find . -type f -print0 | while IFS= read -r -d '' f; do
	mkdir -p "$stage/corpus/$(dirname "$f")"
	cp "$f" "$stage/corpus/$f"
done)
cd "$stage/corpus"
qmd init >/dev/null
qmd collection add . --name modes --mask '**/*.md' >/dev/null
qmd update >/dev/null
qmd embed -c modes >/dev/null

for mode in bm25 vector hybrid full; do
	echo "== recording mode $mode"
	dir=$here/sources/replay/$mode
	rm -rf "$dir"
	mkdir -p "$dir"
	(cd "$corpus_src" && find . -type f -print0 | while IFS= read -r -d '' f; do
		mkdir -p "$dir/corpus/$(dirname "$f")"
		cp "$f" "$dir/corpus/$f"
	done)
	qmd collection show modes >"$dir/collection-show.txt" 2>/dev/null
	qmd status >"$dir/status.txt" 2>/dev/null
	qmd --version >"$dir/version.txt" 2>/dev/null

	rules=$(printf '%s' '')
	for i in "${!queries[@]}"; do
		q=${queries[$i]}
		slug=${slugs[$i]}
		case $mode in
		bm25) qmd search --json -n 25 -c modes -- "$q" >"$dir/$slug.json" 2>/dev/null ;;
		vector) qmd vsearch --json -n 25 -c modes -- "$q" >"$dir/$slug.json" 2>/dev/null ;;
		hybrid) qmd query --json --explain --no-rerank -n 25 -c modes -- "$q" >"$dir/$slug.json" 2>/dev/null ;;
		full) qmd query --json --explain -n 25 -c modes -- "$q" >"$dir/$slug.json" 2>/dev/null ;;
		esac
		rules+="    {\"contains\": [\"$q\"], \"stdout\": \"$slug.json\"},"$'\n'
	done

	cat >"$dir/qmd.json" <<JSON
{
  "comment": "Recorded qmd 2.5.3 output for mode $mode. Regenerate with eval/packs/qmd-modes/record.sh; never edit by hand.",
  "invocations": [
    {"contains": ["--version"], "stdout": "version.txt"},
    {"contains": ["collection", "show"], "stdout": "collection-show.txt"},
    {"contains": ["status"], "stdout": "status.txt"},
$rules    {"contains": ["update"], "stdout": "version.txt"}
  ],
  "default": {"stderr": "no recorded response for this query", "exit_code": 1}
}
JSON
	printf '{\n  "now": "2026-07-29T12:00:00Z"\n}\n' >"$dir/clock.json"
done

echo "== substituting machine paths"
python3 - "$here/sources/replay" "$stage" <<'PY'
import os, sys
root, stage = sys.argv[1], sys.argv[2]
for base, _, names in os.walk(root):
    for name in names:
        if not name.endswith((".txt", ".json")):
            continue
        path = os.path.join(base, name)
        with open(path, encoding="utf-8") as fh:
            body = fh.read()
        body = body.replace(os.path.join(stage, "corpus"), "${REPLAY_DIR}/corpus")
        if stage in body:
            raise SystemExit(f"{path} still names the staging directory")
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(body)
PY

echo "== done; now run the pack and read every case that moved:"
echo "   bin/recall eval run --pack eval/packs/qmd-modes --output /tmp/qmd-modes"

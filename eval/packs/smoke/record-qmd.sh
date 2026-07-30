#!/usr/bin/env bash
# Record the smoke pack's qmd output for the semantic head-to-head.
#
# The pack's paraphrase-semantic cases measure the questions a lexical index
# cannot answer: their answering passage shares at most one content term with
# the question, so the documents source scores near zero on all of them by
# construction. Those numbers only mean something next to a source that can
# reach the passage, which is what this recording is for — the same corpus, the
# same questions, the same graded passages, retrieved by embedding similarity.
#
# It replays rather than spawns qmd. A committed pack runs under
# network_access:false, qmd needs about 2GB of models a CI machine has no
# business downloading, and even warm the tool is a model rather than a
# function. `replay` is the settled spelling the runner's network policy reads.
#
# mode=vector is the recorded mode, and it is the only mode that belongs beside
# a lexical source: it has an admission floor of its own — a query the corpus
# has nothing about does not clear qmd's cosine threshold and comes back `[]` —
# while hybrid and full are rank-normalized and answer every query with
# something. docs/qmd-adapter.md states that at length. The abstention case in
# this pack is the assertion that the floor is real.
#
# The replay directory ships its own copy of the notes corpus. That is not a
# duplicate for its own sake: the adapter compares the directory its collection
# indexes against the configured location on every operation, expansion reads
# evidence from the file rather than through qmd, and the recorded
# `collection show` names that directory through a ${REPLAY_DIR} token. The copy
# is written from sources/notes by this script and must never be edited by hand.
#
# Usage: eval/packs/smoke/record-qmd.sh [staging directory]
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
corpus_src=$here/sources/notes
replay=$here/sources/replay/qmd-vector
stage=${1:-$(mktemp -d)}

command -v qmd >/dev/null || { echo "record: qmd is not on PATH" >&2; exit 1; }

# qmd's own configuration and index stay in the staging directory. A recording
# made against the operator's ~/.cache/qmd would index whatever else that
# machine has collected, and a fixture is supposed to be a closed corpus.
export QMD_CONFIG_DIR=$stage/qmd-config
mkdir -p "$QMD_CONFIG_DIR"

# The questions this pack asks the semantic source. Each is recorded under its
# own slug and the replay rules match on the query text, so one directory
# answers every case. The last one has no answer in the corpus on purpose.
queries=(
	"who services the furnace once a year"
	"when do i take the rubbish out"
	"how do i keep the hose from cracking in winter"
	"the clothes come out soaking wet every time"
	"what did the chimney sweep charge us"
)
slugs=(boiler bins frost laundry chimney)

copy_corpus() {
	local dest=$1
	(cd "$corpus_src" && find . -type f -name '*.md' -print0 | while IFS= read -r -d '' f; do
		mkdir -p "$dest/$(dirname "$f")"
		cp "$f" "$dest/$f"
	done)
}

echo "== indexing the notes corpus in $stage"
rm -rf "$stage/corpus"
mkdir -p "$stage/corpus"
copy_corpus "$stage/corpus"
cd "$stage/corpus"
qmd init >/dev/null
qmd collection add . --name notes --mask '**/*.md' >/dev/null
qmd update >/dev/null
qmd embed -c notes >/dev/null

echo "== recording mode vector"
rm -rf "$replay"
mkdir -p "$replay"
copy_corpus "$replay/corpus"
qmd collection show notes >"$replay/collection-show.txt" 2>/dev/null
qmd status >"$replay/status.txt" 2>/dev/null
qmd --version >"$replay/version.txt" 2>/dev/null

rules=""
for i in "${!queries[@]}"; do
	q=${queries[$i]}
	slug=${slugs[$i]}
	qmd vsearch --json -n 25 -c notes -- "$q" >"$replay/$slug.json" 2>/dev/null
	rules+="    {\"contains\": [\"$q\"], \"stdout\": \"$slug.json\"},"$'\n'
done

cat >"$replay/qmd.json" <<JSON
{
  "comment": "Recorded qmd 2.5.3 vsearch output over the pack's notes corpus. Regenerate with eval/packs/smoke/record-qmd.sh; never edit by hand.",
  "invocations": [
    {"contains": ["--version"], "stdout": "version.txt"},
    {"contains": ["collection", "show"], "stdout": "collection-show.txt"},
    {"contains": ["status"], "stdout": "status.txt"},
$rules    {"contains": ["update"], "stdout": "version.txt"}
  ],
  "default": {"stderr": "no recorded response for this query", "exit_code": 1}
}
JSON
printf '{\n  "now": "2026-07-29T12:00:00Z"\n}\n' >"$replay/clock.json"

echo "== substituting machine paths"
python3 - "$replay" "$stage" <<'PY'
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
echo "   bin/recall eval run --pack eval/packs/smoke --output /tmp/smoke"

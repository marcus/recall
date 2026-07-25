# recall-notes — an external adapter template

A complete Recall adapter in one dependency-free Python file, with the eleven
conformance transcripts that prove it honors the contract. Copy the directory,
replace the parts that are about notes, and keep the parts that are about the
protocol.

It serves a deliberately dull source — a directory of Markdown notes with a
small header block — so that nothing in it is interesting except the contract:
version negotiation, an index that lives only in the workdir, locators that
survive a source change, coverage that is stated rather than implied, and a
source that names the store it opened.

Read [`docs/writing-an-adapter.md`](../../docs/writing-an-adapter.md) alongside
it. That document explains why each of these decisions is the way it is and
what failure it prevents; this directory is the same thing you can run.

## What is here

```text
recall_notes.py        the adapter: six handlers, ~1500 lines, most of it
                       commentary explaining what each decision prevents
conformance.py         record and replay the transcripts, with nothing but Python
conformance/<case>/    manifest.json, request.jsonl, response.jsonl, fixture/
```

The adapter needs Python 3.9 or later and the standard library. `sqlite3` is
used for the index; no FTS5 extension is required, and swapping the term table
for FTS5 changes nothing about the protocol.

## Run it by hand

The transport is newline-delimited JSON-RPC on stdin and stdout, so the whole
thing is debuggable with a shell:

```sh
python3 recall_notes.py <<'EOF' | jq .
{"jsonrpc":"2.0","id":1,"method":"recall/initialize","params":{"protocol_version_min":1,"protocol_version_max":1,"workdir":"/tmp/notes-work","source_id":"notes","location":"conformance/handshake/fixture","settings":{"notes_dir":"notes"}}}
{"jsonrpc":"2.0","id":2,"method":"recall/search","params":{"query":"coverage honesty","filters":{},"limit":5,"deadline":"2099-01-01T00:00:00Z"}}
EOF
```

`mkdir -p /tmp/notes-work` first: the workdir is supplied by the core in real
use and must exist and be writable.

## Verify it

Three ways, in increasing order of authority.

```sh
python3 conformance.py verify          # needs only Python
recall doctor --conformance notes      # replays through Recall's own engine
```

The third is the Recall test suite: `templates/conformance_test.go` in the
Recall tree replays this same directory on every `make check`, which is what
keeps the recording in this repository honest.

`recall doctor --conformance` needs the adapter registered in **user**
configuration — `$XDG_CONFIG_HOME/recall/config.toml` — because an adapter
command may only be declared there. Both paths must be absolute:

```toml
[adapters.notes]
command = "python3"
args = ["/absolute/path/to/recall_notes.py"]
freshness_modes = ["indexed"]
conformance = "/absolute/path/to/conformance"
```

A source instance then names the adapter and points at a corpus:

```toml
[[sources]]
source_id = "notes"
adapter = "notes"
location = "/home/you/notes"
freshness_mode = "indexed"
settings = { notes_dir = "" }
```

## Change it, then re-record

The recorder is the replayer: a transcript is an observation of the adapter,
never a claim about it. After changing behavior:

```sh
python3 conformance.py record          # rewrites every response.jsonl
git diff conformance/                  # read this. every line is a decision
python3 conformance.py verify
python3 -m unittest -v test_recall_notes.py
```

`record` refuses a case whose run produced a different number of frames than
its `manifest.json` declares, so adding or removing a request means updating the
manifest deliberately. Read the diff before committing it — a re-recording that
was not read is a test that now asserts whatever the code happens to do.

When you add a case, write its `description` first. The loaders on both sides
refuse an empty one, because a transcript nobody can read is not documentation.

## What to keep when you copy

Keep, and change only with a reason you can write down:

- `store_identity` naming the store you **opened**, hashed. Two instances of one
  adapter reporting one value is a configuration error nothing else can see.
- `content_fingerprint` over every material record field — title, heading,
  tags, date, sensitivity, aliases, body, and lineage — and over nothing about
  where this instance found it.
- `source_record_id` being the record, while `candidate_id` is the hit. Several
  sections of one note are several candidates and one piece of evidence.
- Every path that returns `partial`, and every diagnostic that says why. There
  are four in here: an unreadable record, a truncated listing, a filter that
  could not be applied, and a source that could not be reached at all.
- The refusal to derive an event time from a file's mtime.
- `one_line` and `safe_text` on every string that leaves the process.
- Byte limits that include the UTF-8 ellipsis, rather than appending it after
  the budget has already been spent.
- A durable checkpoint publication pointer for immutable digest-bound index
  generations; a failed checkpoint keeps the prior generation answering.
- The pre/read/post scan boundary: a source that changes while it is being read
  publishes nothing, rather than pairing stale bytes with a fresh cache key.
- Post-rename checkpoint semantics: uncertain directory fsync degrades the newly
  published generation but never leaves live state disagreeing with restart.
- `safe_basename` on every file or directory name that reaches health, errors,
  or stderr. A base name is source text and can carry CR or ANSI controls.
- The `--version` flag and the stderr-only logging.

Throw away: the note format, the header keys, the section splitting, the field
weights, and `debug_stall_ms` — which exists only so the cancellation case can
be recorded deterministically, and which you should keep only if you also want
that case.

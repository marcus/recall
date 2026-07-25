# Writing A Recall Adapter

This is the working document for someone writing an external adapter — a
process Recall spawns, in any language, that answers "which of my records best
match this request?" over JSON-RPC on stdio.

[adapter-protocol.md](adapter-protocol.md) is the normative contract: message
shapes, codes, field names. It says what is legal. This says what is **right**,
which is a smaller set, and it is organized around the mistakes that are easy to
make and expensive to ship. An adapter that gets the things below wrong is worse
than no adapter at all, because a source that answers confidently and wrongly
displaces the sources that would have answered correctly.

The companion artifact is [`templates/adapter-python`](../templates/adapter-python):
a complete adapter in one dependency-free Python file, with recorded conformance
transcripts. Copy it. Everything this document argues for is implemented there,
and the transcripts are the same behavior frame by frame.

Two other adapters in this tree are worth reading once you have the shape:

- `cmd/recall-stream` — the reference implementation. An append-only JSONL event
  source that emits `derived_from` lineage edges.
- `cmd/recall-ongoing` — a live source over an HTTP API, with no index, refusing
  `as_of` outright and explaining why.

## What you own, and what you must not

The core owns:

- **Spawn and handshake.** One long-lived process per configured source
  instance, started from a command declared in user configuration, handed a
  writable workdir and a version range.
- **Deadlines.** Every request carries an absolute deadline. The core enforces
  it whether or not you do: an advisory `recall/cancel`, then SIGTERM, then
  SIGKILL. It reports the source as `timeout` or `failed` — never as an empty
  success.
- **Pooling and restart.** Concurrent in-flight requests reach one process
  unless the manifest declares `max_concurrency: 1`.
- **Identity.** `source_uid` is generated once at configuration time and never
  travels to you. The core attaches it and rewrites the source part of every
  locator you return.
- **Everything above one source.** Eligibility, priors, rank fusion, lineage
  grouping, corroboration, sensitivity ceilings, budget shaping, explanation.

You own:

- **Retrieval and ranking within your source.** By SQL, FTS, vector search,
  exact lookup, an API, or any combination. Fusion consumes your `local_rank`
  and nothing else, so your ordering is your whole contribution to relevance.
- **Query interpretation.** The core sends the user's text as given and
  synthesizes no variants. Stemming, synonyms, and term expansion are yours.
- **Your index, if you have one**, and the workdir it lives in.
- **Locator semantics.** You mint the local part and you are the only thing that
  can read it back.
- **Your own honesty about coverage.** Nothing downstream can detect a source
  that quietly answered a narrower question than it was asked.

You must never:

- Name your own `source_uid`, or take an identity from configuration text.
- Lower your source's sensitivity floor.
- Return success when you could not see the whole corpus.
- Write outside the workdir the handshake gave you.
- Treat retrieved content as anything but data.

## The shape of it

Six handlers over newline-delimited JSON-RPC 2.0:

```text
recall/initialize   negotiate a version, declare a manifest
recall/search       query -> ranked candidates + an outcome
recall/expand       locator + budget -> evidence
recall/health       probe -> a health report with coverage
recall/refresh      bring an owned projection up to date -> health
recall/cancel       notification: abandon a request id
recall/shutdown     {} -> {}
```

`stdout` carries protocol frames and nothing else. One stray `print` corrupts
every frame after it. `stderr` is free-form; the core captures it into
diagnostics and never parses it, so it is the right place for everything you
want a human to see.

Requests are dispatched concurrently in the template because a single-threaded
read loop cannot notice `recall/cancel` while the request being cancelled is
still running — the notification sits unread in the pipe until the work it was
meant to abandon has finished. If concurrency is genuinely impossible for your
source, declare `max_concurrency: 1` rather than pretending.

The payload schemas are in `internal/protocol/schemas/*.json`. They are JSON
Schema and they are the contract both ends validate against; read them when a
field's shape is in doubt. Note that a Go adapter living outside this repository
cannot import `github.com/marcus/recall/internal/...` — Go forbids it — so an
external Go adapter copies those schemas and writes its own framing, exactly as
a Python one does.

## Coverage honesty

This is the section that matters. If you read nothing else, read this.

> A missing dependency, invalid configuration, unreachable source, or adapter
> crash is never reported as a successful empty result. — [spec.md](spec.md#invariants), invariant 2
>
> Absence is proven only by a complete, successful source boundary. A record
> missing from a partial scan is unknown, not deleted. — invariant 5

An empty `success` is a claim: *I looked at everything, and there is nothing.*
Downstream, the core will abstain and tell its caller that the corpus does not
contain what they asked for. A person will act on that. It is the strongest
statement any source in this system can make, and the only thing standing
between it and a false absence is your adapter's willingness to say `partial`.

There is exactly one situation in which an empty candidate list may carry
`outcome: "success"`: you searched a complete, current view of the corpus and
nothing matched. Every other empty list is `partial`, `unavailable`, `denied`,
`failed`, or `timeout`.

### The shapes this takes

**A failed probe.** The database is not there, the endpoint refuses the
connection, the credentials expired. Return the error — `source_unavailable`
(-32000) or `source_denied` (-32001) — and let `recall/health` report `status:
unavailable` with `coverage: unknown`. Do not return zero candidates. The
template's `search-unavailable` transcript is this case: the handshake succeeds,
because the configuration is valid and nothing has been asked yet, and the
search then fails explicitly.

**A truncated listing.** You capped the scan, the API paginated and you stopped,
the directory holds more files than your limit. Nothing about the candidate list
shows it: the records you did read are read correctly and rank correctly. Say so
— `outcome: "partial"`, a diagnostic naming the truncation, `coverage: partial`
in health. See `search-truncated`.

**Records you could not read.** A row that failed to parse, a file with a bad
encoding, a document your extractor choked on. Count them. `record_count` is
what the source holds; `indexed_count` is what reached the index; `failed_count`
is the difference, and it is the number that turns "we found two" into "we found
two of four". One bad record must not take the source down, and it must not
vanish either. See `search-partial`.

**A filter you cannot apply.** The request narrows by entity or by project and
your source has no such concept. You have three options and two of them are
wrong: applying it by guessing invents matches, and dropping every candidate
because you cannot prove a match manufactures an absence. What is left is to
answer the broader question and say plainly that you did — `outcome: "partial"`
with the unapplied filters named in diagnostics.

This last one is a rough edge in v1 and worth knowing about: unlike `as_of`,
where the manifest declares `none | filter | snapshot` and the core excludes
sources that cannot honor a boundary, there is no capability flag for filter
support. A `partial` outcome plus a named diagnostic is the honest encoding
available today. See `search-filtered`.

### What health has to agree with

A search that reports `partial` and a health probe that reports `healthy` with
`coverage: complete` cannot both be true. The core reads both. Keep them derived
from the same snapshot, as the template does — one `coverage` property computed
once, consumed by both.

A recent index timestamp is not health. `status: healthy` on a partial index is
only permissible when your declared `freshness_policy` names that exact partial
boundary as acceptable; if it does not, the status is `degraded`.

## Identity comes from what you opened

Lineage groups on `source_uid` plus `source_record_id`. When one record reaches
the core through two configured instances of one adapter, it arrives as two
independent pieces of evidence — and the corroboration bonus then promotes it
for agreeing with itself. Nothing downstream can see the duplication, because by
that point the two instances are two legitimate sources.

This has now happened three times in this system, found each time only after it
had been shipping: two document sources over overlapping roots, two catalog
instances over one server, and two `td` sources whose configured locations
resolved to one SQLite database. Read commit `e2d42d3` for the last one; it is
the clearest worked example in the tree. `td` resolves its database by walking
*upward* from the directory it is given, so a repository and any subdirectory of
it open the same file. Identity was `filepath.Base(location)`, so the two
instances called themselves `recall` and `docs`, `recall doctor` reported ok, and
the core counted one issue in one file as two pieces of evidence.

The rule that falls out of it:

> Derive identity from what the source actually opened, never from what
> configuration said.

A value copied from configuration compares equal exactly when the configuration
is already consistent, which is the case that was never in doubt.

### `store_identity`

Publish, under `diagnostics.store_identity` in your health report, an opaque
value naming the store **this instance opened**. `recall doctor` fails a profile
in which two enabled instances of one adapter report the same value.

Three properties make it worth publishing:

- **It names what was opened.** A resolved path, a database identity, an
  endpoint plus a namespace. The template uses the realpath of the directory it
  listed, so a source configured at a directory and another at a symlink to it
  compare equal.
- **Setting it claims exclusivity.** "This store is mine alone." An adapter for
  which two instances over one store is a legitimate design — serving two views
  of a shared catalog, whose candidates collapse on a `content_fingerprint` —
  leaves it unset. Absent means *makes no such claim*, never *unknown*.
- **It is compared only for equality, within one adapter.** Which means it can
  be hashed, and should be. The template publishes
  `"dir:" + sha256(realpath)[:16]`. The check works identically, and a digest
  cannot leak a home directory into a diagnostic, a log, or a committed
  conformance transcript.

If resolving your store costs a subprocess or a network call, publish it only
once you have confirmed the resolution — `td` compares its own answer against
what `td info` reports and degrades the source when they disagree, rather than
quietly answering for the wrong workspace.

### `content_fingerprint`

An adapter that claims a store should also carry a fingerprint on its
candidates, so that a duplicate configuration is corrected by the operator *and*
harmless until they get to it. Two candidates with the same fingerprint collapse
for corroboration counting.

Build it from the record's own identity and content and from **nothing about
where this instance found it**. The location, the watermark, the generation, and
the store identity are precisely what two instances over one store disagree
about; a fingerprint built on any of them differs for the same record and
defeats its own purpose.

### `source_record_id` is the record, not the hit

`candidate_id` identifies this hit. `source_record_id` identifies the *record*.
Chunks of one document, several sections of one note, several matches inside one
task all carry the same `source_record_id`. Corroboration collapses on it, so a
per-chunk value lets one source corroborate itself — the same defect as the
duplicate-instance case, reached from inside a single adapter.

### Locators

Locator text is `<source_id>:<local>`, and you write your own. The `source_id`
arrives at the handshake for exactly this reason. It is a name, not an identity:
the core overwrites the source part of every locator you return, so a forged
prefix cannot make one source answer as another. Only a `derived_from` prefix is
read at face value, and an unknown one is dropped.

The local part is yours, and it is a promise. `notes:note-ranking#3` promises
that expanding it returns section 3 of that note. When the source changes so that
the promise cannot be kept, fail with `locator_expired` (-32003). Never return a
neighbouring record, a different revision, or the closest thing you have — that
is the substitution the protocol exists to forbid, and it is indistinguishable
to the caller from a correct answer.

Distinguish the two failures. A local part you cannot parse at all is
`locator_unknown` (-32002): a statement about the reference. A local part you can
read that no longer resolves is `locator_expired`: a statement about the source.

### Lineage across sources

If your records are projections of another source's records — a signal stream
carrying task events, a mail archive carrying calendar invitations — declare
`derived_from` on each candidate, naming the upstream record's locator in the
upstream source's own display form. `cmd/recall-stream` does this: a signal
about task `td-f62256` declares `tasks:td-f62256`, character-for-character the
locator the Tasks adapter writes, so the two collapse into one lineage root and
never corroborate each other.

Emit no edge when you cannot map the upstream system to a configured
`source_id`. A guessed edge resolves *somewhere*, and a wrong lineage root is
worse than a missing one.

## Freshness and watermarks

A watermark answers: *what did this answer see?* It is a string, it is opaque to
the core, and its only job is to be comparable — between two searches, between
search and health, and between two machines indexing the same corpus.

A good watermark is derived from the source. The template's is
`notes=3 sections=4 latest=2026-05-20T11:15:00Z digest=9445a77c…`: counts, the
newest event time, and a digest over each record's identity and content.

A bad watermark:

- **is a wall clock.** "Indexed at 14:02" tells a caller when you ran, not what
  you saw. Two runs over an unchanged corpus produce different watermarks and a
  caller comparing them concludes something changed.
- **contains anything about this machine.** An mtime, an inode, a path. Two
  hosts indexing the same corpus then disagree, which is exactly when a caller
  most needs to see that they agree — and it puts local filesystem layout into
  something that gets logged and committed.
- **is a row count alone.** Insert one record and delete another and it does not
  move.
- **is unstable across a rebuild.** If reindexing an unchanged corpus produces a
  different watermark, the value cannot be used to decide whether a rebuild is
  needed.

Mtimes and sizes are fine to *read* — the template uses them to decide whether
to rebuild — as long as they never reach a field anyone else sees.

### If you own an index

Declare `capabilities: ["checkpoint"]` and serve `recall/refresh`. That method is
the contract's only entry point for building an index outside the handshake, and
you want it: building inside `recall/initialize` competes with the core's
10-second handshake timeout on any real corpus.

The obligations, in the order they matter (full list in
[spec.md](spec.md#index-obligations)):

- **The index is a rebuildable projection, never the source of truth.** Deleting
  Recall's state directory changes latency, never results.
- **Build into a new generation and publish atomically**, only after the source
  boundary completes. The template builds `build-N.sqlite` and `os.replace`s it
  over `index.sqlite`. A build that fails leaves the previous generation
  published, readable, and answering.
- **Checkpoint after, never before.** Records durable, then the file that names
  them. A checkpoint that names a generation the reader cannot open is worse
  than no checkpoint.
- **Report `index_generation`, `index_watermark`, and `index_config`.**
  `index_config` identifies the retrieval configuration — analyzer, tokenizer,
  scoring parameters — and exists because `index_model` covers only embeddings.
  Without it, a change to your scoring silently moves ranking with nothing in
  the generation recording it, and an evaluation comparing two generations
  credits the difference to whatever else was under test. Change the string
  whenever tokenization or scoring changes.
- **A failed refresh returns health, not an error.** A JSON-RPC frame carries a
  result or an error and never both, so erroring discards the health of the
  generation that is still published and still answering. Report the failure
  through the health you return — stale watermark, `degraded`, the reason in
  diagnostics. An error return means the refresh could not be attempted at all.

## Time

Four timestamps, never collapsed into one:

| field | question it answers |
| --- | --- |
| `event_time` | when the underlying event happened |
| `valid_from` / `valid_to` | when a fact was or is true |
| `observed_at` | when Recall read or indexed the record |
| `confirmed_at` | when a complete successful source boundary confirmed it |

`observed_at` alone proves nothing about a later boundary — that is what
`confirmed_at` is for. In an adapter where one scan does both they are the same
instant, and they are still reported separately, because for a source with an
incremental boundary they are not.

Do not derive an event time from a file's mtime. An mtime is a property of this
checkout, not of the record: the same corpus indexed on two machines would carry
two different event times, and a recorded conformance transcript would stop
matching the moment someone cloned the repository. The template refuses a note
with no `date:` header rather than guessing one — and counts the refusal, which
is how the refusal stays visible.

### `as_of`

Declare one of three, honestly:

```text
none      you cannot answer historical questions
filter    you can restrict by event or validity time you already store
snapshot  you can reconstruct source state at a past instant
```

A request carrying `as_of` excludes sources declaring `none`, reports them with
reason `as_of_unsupported`, and degrades coverage. That is the system working:
being excluded is a correct outcome, and it is much better than answering.

`filter` is the common honest answer for a source whose records carry immutable
event times. The template declares it: restricting to notes written at or before
a boundary is a filter over history the corpus already stores.

`snapshot` is a strong claim and usually a lie. The template refuses it because
the corpus keeps no revision history — the set of notes present at a past instant
cannot be reconstructed from current state. `cmd/recall-stream` refuses it too,
for a subtler reason worth internalizing: a record describing an early event can
be appended at any later time, so the set of records in the file at a past
instant is not the set an event-time filter selects, and the format publishes no
append time to reconstruct it from.

Never answer an `as_of` query from current state. `cmd/recall-ongoing` declares
`none` rather than filtering on `latestCommitAt`, which would answer a historical
question from a field describing now.

## Sensitivity, excerpts, and trust

Sensitivity is an ordered scale: `public < internal < confidential < restricted`.
Your manifest declares the source's floor; configuration may raise it. A
candidate may raise its own classification above the floor and **may never lower
it** — the only operation an adapter performs on sensitivity is a maximum.

An excerpt is a bounded preview, not a payload. A candidate is a pointer; the
locator is how a caller gets the rest. Bound it in bytes, cut at a character
boundary, and mark that it was cut.

Everything you emit is untrusted source text on its way to a terminal and to a
model, and the core's sanitizer only walks top-level string fields — anything
nested one level down, such as a list inside `metadata`, arrives exactly as your
source stored it. Sanitize at the edge of your own process:

- **Single-line fields** — title, headings, provenance, every string in metadata
  — collapse to one line. A newline in a title forges a section header in
  evidence a model reads.
- **Multi-line evidence** keeps its newlines and loses everything else: C0 and C1
  controls carry ANSI colour and cursor movement that a terminal obeys; bidi
  overrides and isolates reorder what a reader sees without changing what a
  program matched; U+2028 and U+2029 are line breaks that most whitespace
  splitters do not recognize.
- **Keep types.** A null stays null; a string that happens to look like a number
  stays a string.

Write those character classes as escapes in your source. A file that spells them
out carries an invisible bidi override and breaks the next tool that reads it.

For diagnostics: a denied source must not leak record existence, and no
diagnostic needs an absolute path. Name files by base name. Hash anything that
is a path by nature.

### The trust boundary

Adapter commands, argv, and environment may be declared **only in user-level
configuration** (`$XDG_CONFIG_HOME/recall/config.toml` and `adapters.d/`). A
project configuration travels with a cloned repository; loading one must never
be able to execute attacker-chosen code. A project file also may not change
`location`, `settings`, or `enabled` on a source the user layer declared —
`settings` is adapter-owned and unvalidated at load, so a key like `cli` could
name a program without ever looking like an executable key. See
[spec.md](spec.md#layers-and-trust-boundary); `recall doctor` fails loudly on
every rule there.

Two consequences for you. Your settings block is the sharpest edge in your
adapter: it is the one place configuration reaches your code unchecked, so
validate it yourself, and treat any path in it as hostile until you have
resolved it and confirmed it stays inside the location. And open your source
read-only unless writing is the point.

## Settings schemas

Return `settings_schema` from `recall/initialize`. It arrives in the *result*,
after the core has already sent you `settings` — so on a first handshake you
validate your own settings and fail the handshake with a readable error, and on
every later one the core validates against the schema it already holds. `recall
doctor` uses it to check configuration without starting a query.

Two rules:

- **Every declared key has code reading it.** A key in the schema with no code
  path behind it is configuration that appears to work and does nothing, and it
  will be set by someone who then believes it took effect.
- **Unknown keys are rejected.** A misspelled setting that silently did nothing
  is the same defect from the other direction. Fail the handshake and name the
  key.

Describe each key with what it does and what happens at its boundary, not with
its type — the type is already in the schema. `recall config explain` prints
these descriptions to a person deciding what to set.

Paths inside settings are yours to resolve, against `location` or against
`base_dir` (the directory of the file that declared the source). `base_dir`
exists because configuration cannot resolve paths inside an adapter-owned block;
an adapter that resolved against the process working directory made the same
configuration read different files depending on where Recall was started.

## Conformance

Conformance is recorded transcripts, replayed. Ship them, or your adapter is
reported as unchecked rather than as passing — "verified nothing" must never
read as "verified".

```text
conformance/
  <case>/manifest.json      what the case is, and how to replay it
  <case>/request.jsonl      one JSON-RPC message per line, order-significant
  <case>/response.jsonl     what the adapter wrote, order-significant
  <case>/fixture/           source data this case is configured against
```

The format is specified in `cmd/recall-stream/conformance/FORMAT.md` and is not
specific to that adapter. Register the directory as an absolute path in your
adapter's user-level configuration and replay it with:

```sh
recall doctor --conformance <adapter>
```

### The required cases

From [adapter-protocol.md](adapter-protocol.md#conformance):

- Handshake, including a version-range rejection.
- Search returning ranked candidates with stable locators.
- Search returning `partial` with honest coverage.
- Search against an unavailable source.
- Expand at each detail level, including a budget truncation.
- Expand with an expired locator.
- Cancellation of an in-flight request.
- Clean shutdown.

The template ships eleven: those eight, with the version rejection recorded
separately, plus a truncated listing and an unapplied filter — the two coverage
shapes nothing else in the tree demonstrates.

### Record; never write by hand

A hand-written transcript is a claim about your adapter. A recorded one is an
observation of it. Drive the real process, capture what comes back, and commit
that:

```sh
python3 conformance.py record     # in the template
```

Then **read the diff**. A re-recording nobody read is a test that now asserts
whatever the code happens to do. The recorder refuses a case whose run produced a
different number of frames than the manifest declares, so adding a request means
updating the manifest deliberately rather than discovering the change later.

### `volatile`, and how little of it you need

`volatile` lists JSON Pointers, from the root of a response *frame*, whose values
are masked on both sides before comparison. `*` matches every element of an array
or member of an object, so `/result/candidates/*/confirmed_at` covers a whole
list in one declaration.

Declare a field volatile **only when your adapter genuinely cannot control it**:
wall-clock timestamps, latencies, process identity. Everything else is the
contract under test. A timestamp the fixture states is not volatile — an adapter
that invented one instead of reading it should fail.

The template declares six. Five are clock readings — `checked_at`,
`last_success_at`, `observed_at`, `confirmed_at`, `elapsed_ms`. The sixth is
`store_identity`, which is derived from `${FIXTURE}` and so differs on every
machine that checks the corpus out; masking it is right, and hashing it is
what keeps the recorded
value from being a path. `event_time` is not among them: the fixture states it,
and an adapter that invented one should fail.

### Transcripts carry no machine paths

Requests carry `${FIXTURE}` and `${WORKDIR}`, substituted textually before
parsing, because a handshake carries two absolute paths that differ on every
machine. There is no `${NOW}`: every recorded request states a fixed far-future
deadline, so a transcript replays identically today and in ten years without the
harness rewriting request fields.

The subtler half is the **responses**. Anything your adapter derives from those
paths — a resolved root, a store identity, a provenance string, a diagnostic
naming a file — lands in a file you commit. Masking it with `volatile` keeps the
replay green and still leaves someone's home directory in the repository. This
tree has an example: `internal/adapters/td/conformance/handshake/response.jsonl`
carries `workspace_root` and `workspace_location` as absolute paths, declared
volatile. It replays correctly and it should not have been committed that way.

Two habits avoid it. Name files by base name in every diagnostic. And hash any
value that is a path by nature, including `store_identity` — the checks that
consume these compare for equality and never read them.

### One workdir per case, per run

`${WORKDIR}` must bind to a fresh, empty, writable directory for each case and
each run. Reusing one lets a second replay observe a warm index the recording
never had, and the case silently stops testing a cold start.

## Errors and outcomes

```text
-32000  source_unavailable   cannot reach the source
-32001  source_denied        permission refused; leak nothing about records
-32002  locator_unknown      the local part does not parse for this adapter
-32003  locator_expired      the source changed incompatibly
-32004  source_not_configured
-32005  as_of_unsupported    historical boundary cannot be honored
-32006  budget_exceeded      declined up front: cannot serve in this budget
-32007  deadline_exceeded    ran out of time, or was abandoned in flight
```

`budget_exceeded` and `deadline_exceeded` are different facts. The first is a
refusal before work started; the second is work that ran out of time or was
cancelled. Conflating them made one timeout report as `SearchTimeout` in process
and `SearchFailed` over the wire.

Search outcomes: `success | partial | unavailable | denied | failed | timeout |
skipped`. The core rejects an outcome it does not recognize and treats it as a
failure, never as an implicit success.

On cancellation, the whole of what you owe is: notice, return, and do not
answer. A late result is worse than an error, because the core has already told
its caller that this source did not answer.

## Before you ship

- [ ] Every path that returns an empty candidate list is a path where you saw
      the whole corpus.
- [ ] Health and search agree about coverage, derived from one snapshot.
- [ ] `store_identity` names what you opened, hashed — or is deliberately unset
      because two instances over one store is your design, in which case your
      candidates carry a `content_fingerprint`.
- [ ] `source_record_id` is the record; `candidate_id` is the hit.
- [ ] No `source_uid` anywhere in your output.
- [ ] The watermark is derived from the source and is stable across a rebuild of
      an unchanged corpus.
- [ ] `as_of_support` is the weakest of the three that you can actually honor.
- [ ] Every settings key has code reading it; unknown keys are rejected.
- [ ] Every string that leaves the process is sanitized, including nested ones.
- [ ] No diagnostic, provenance string, or transcript contains an absolute path.
- [ ] You write only inside the workdir.
- [ ] The eight required conformance cases are recorded, and you have read the
      diff of the last recording.
- [ ] `recall doctor --conformance <adapter>` passes from a clean checkout.

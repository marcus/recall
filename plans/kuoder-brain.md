# kuoder/brain, next to Recall and Clara memory

Cloned 2026-08-12 to `~/code/brain` from `git@github.com:kuoder/brain.git`
(`a2700ab`). Clara memory is the tree at `~/code/clara` (`lib/clara/memory`,
`lib/clara/observations.rb`, `docs/data-contracts.md`). This is a research
note, not a spec.

Three products share a verb. They do not share a job.

Brain decides what an agent should believe next session, and it owns the
rows that answer. Recall finds evidence in stores it does not own. Clara
memory is the write-owning store on this machine: inspectable JSONL, half-life
decay, and preferences rebuilt from observations. The Recall spec already
points at that split twice. It does not decide what becomes durable memory,
and the core never implements decay.

The useful comparison is which jobs each one took, which contracts they
would break by taking another's, and which ideas travel.

## What each one is

Brain is a memory service. Agents write `engram` rows through fourteen MCP
tools. A SessionStart hook injects a token-budgeted brief. Nightly jobs decay
Hebbian weights, distill hub clusters into `lesson` engrams, promote
cross-project abstractions into a shared tier, and run Sonnet NLI against
near-neighbors so contradictions invalidate the older belief instead of
deleting it. Commits, files, features, worktrees, and failed fix attempts are
the same kind of row as decisions. A Vite UI, embedded in `braind`, lets a
human walk the graph. The store is PostgreSQL 18 + pgvector. Embeddings are
local (`nomic-embed-text` via Ollama). Consolidation and contradiction call
Anthropic. One canonical daemon on the Studio, reached over the tailnet.

Recall is a read-only retrieval layer over sources that already exist. A query
fans out to whatever the active profile configured — documents, td, Tasks,
Gmail, qmd, a Clara corpus — fuses their ranked lists without comparing native
scores, and returns locators. Expansion is a second call. The core owns no
index and no records. Deleting `~/.local/state/recall` changes latency, never
results. `go.mod` is toml plus jsonschema. CLI, loopback HTTP, and MCP are
thin shells over one application layer.

Clara memory is a typed JSONL store the CLI alone may write. Live rows live
in `data/memory.jsonl`; faded and superseded rows go to
`data/memory-archive.jsonl`. A record is a kind, a stable `subject`, a body,
a weight, and an optional half-life. Facts and feedback do not decay.
Preferences default to 45 days, momentary `signal` rows to 10. Effective
weight is `weight × 0.5 ** (days_since_last_seen / half_life)`. The same
subject written again reinforces: weight up (capped at 1.0), `last_seen`
today, `hits` incremented. `clara memory recall` is lexical AND over whole
tokens, with subject/title/tag matches outranking a body hit, and decayed
weight as the tie-break. It does not call Recall. Observations
(`acted` / `dismissed` / `reprioritized`) are a second, append-only file.
Generated preferences are a projection of that file: four distinct refs in
one exact `source + kind` scope, unanimous, latest reaction per ref. Replay
rebuilds the projection without touching a hand-written fact. Check-ins are
a third store: a verified subject that should come back on a cadence,
without pretending a new Slack message arrived.

Joe's decision log puts his boundary in almost our words: Brain owns history
and knowledge; live workflow stays in a future orchestrator. Clara already
drew that line in the other direction. How-to goes in a playbook or a skill.
What-is goes in memory. What the owner did goes in `observe`. Tasks stay in
the day-job CLI. People stay in dex. Recall searches all of it and owns
none.

## The journeys, side by side

**An agent opens a session.** Brain curls `/api/v1/brief` from a SessionStart
hook, fail-open, two-second timeout, and pastes decisions, lessons,
preferences, recently invalidated beliefs the agent may still be carrying,
and anything that arrived on the agent push channel while nobody was logged
in. The agent does not have to remember to search.

Clara's brief is a fetch/ingest/score loop over live sources. Memory is an
input to scoring — a `hot` tag on `project:checkout` moves a matching Jira
ticket, a generated preference adds or subtracts points — not a dump of
every fact into the prompt. `docs/learning-loop.md` forbids unbounded
MEMORY.md-style injection on purpose. A cold-start chat is told to run
`clara memory recall <topic>` (or, on this machine, `recall query` first)
and to treat memory as an index whose bodies point at `projects/<slug>/`.
If the agent skips that step, the facts are sitting in a greppable file
and nobody read them.

Recall has `pre_reply` as a host mode and four MCP tools, and nothing in
this tree injects them. If the host does not call `recall query`, Recall
does not exist for that session.

**An agent needs a fact.** `brain_recall` embeds the query, takes the top 32
currently-valid neighbors by cosine, spreads two hops over Hebbian `w_fast`,
injects a few "after A you usually needed B" transition targets, and scores:

```
s_final = (0.30·sim + 0.20·spread + 0.10·transition
         + 0.25·σ(B) + 0.15·importance) · confidence
```

Every result carries that breakdown. The call also writes: weighted accesses,
transition counts, and pairwise edge strengthening on the top eight. Cosine
always returns a list. There is no abstention.

`clara memory recall dentist` tokenizes the query, requires every term as a
whole token in some field so `DoR` hits and `dormant` does not, averages
per-term field scores, and lets a literal phrase beat the same terms
scattered. Decay breaks ties. A miss is an empty table, not a coverage
claim about any other store. `clara memory list` is the no-query mode:
rank the whole file by effective weight. Neither command writes. Decay is
arithmetic over `last_seen` and today's civil date.

`recall query` asks every eligible source, under a deadline, and fuses by
local rank, a shared relevance definition, and a source prior. On this
machine that set includes Clara's corpus. Clara memory itself is
`confidential`, so the `home` profile (ceiling `internal`) is configured,
asked, and unable to answer — named in `personal` and `memory` instead,
same as calendar and mail. The default answer is pointers. Full evidence is
`recall expand`. A complete miss with every source answering is exit 2. A
result set from an incomplete fan-out is exit 3. A world in which nothing
searched is exit 4. Query does not write.

**An agent learns something.** Brain has `brain_remember`, `brain_decide`,
`brain_revise`, `brain_invalidate`, and `brain_link`. Ingest secret-scans
(pattern plus entropy; the error names the class, never the matched text),
folds exact and near duplicates (cosine ≥ 0.90 reinforces the existing row
and returns `refined_existing`), and enqueues a contradiction check.

Clara splits the write. A stated fact or preference is `clara memory add
--subject S --body B`. The same subject with a different body is refused
unless `--replace` (prior row snapshotted into the archive, tagged
`superseded`) or `--append`. Tags merge. `--dry-run` prints the refusal
without writing. `--at YYYY-MM-DD` dates an import to when the thing was
learned, because decay reads `last_seen || created` and an import stamped
today would lie about a five-month-old preference. A behavioral reaction
is `clara observe <signal-id> acted|dismissed|reprioritized`. That row is
immutable. `last_action` on the signal is projected at read time.
`clara memory consolidate` then merges duplicate subjects, rebuilds only
the generated-preference projection, and archives anything whose effective
weight has fallen below 0.05. Four unanimous observations promote; one
contrary vote blocks. `clara preferences remove` disables the generated
row without deleting the observations. There is no NLI, no secret scan on
the way in, and no `invalidate` that leaves the live row queryable as
false. `clara memory forget` drops the live row. The archive is for
fade and for replace, not for forget.

Recall has no write path.

**A human wants to see the system.** Brain ships Overview, Memories, Review,
Graph, Recalls, Events. Clara ships `clara memory list|recall`,
`clara preferences list|explain`, `clara observations list`, and the two
JSONL files themselves — fixed key order, one line per record, a change is
a one-line diff. Recall ships `recall sources`, `recall doctor`,
`recall config explain`, and `--explain` on a query. Clara's files are the
best of the three for an agent with `rg`. Brain's UI is the best for a
human walking a graph. Recall's diagnostics are the best for "which source
failed."

**Something is on fire on a laptop.** `brainctl doctor` never opens the
database. It checks token presence, daemon reachability, that the token is
accepted, that a deliberately invalid token is *rejected*, that MCP's Host
allowlist admits this machine, and that `$PWD` is a registered project.
The invalid-token probe exists because a working token does not prove
enforcement.

`clara check` asks whether the JSONL is well-formed, whether schemas hold,
and whether the owner's declared hot-project list still matches memory
rows tagged `hot`. It will not tell you that the last three sessions
forgot to `memory add`.

`recall doctor` asks whether the layers loaded, whether the project file
stayed inside the trust boundary, whether every eligible source can be
read, whether anyone is serving a stale or partial corpus, and whether the
must-abstain queries still abstain. With `--conformance` it replays an
adapter's recorded transcripts. `recall doctor --server` does not ask
that server whether a bad token is rejected.

## What Brain does better

**The product is a loop, not a search box.** Write, retrieve, strengthen,
contradict, distill, inject at the next session. Clara has the write
commands and a standing instruction to use them. Brain has the habit: a
fail-open SessionStart hook, a write nudge in the brief that names the
project slug, and unregistered directories that silently no-op everywhere,
with doctor as how you find that out. Clara's learning-loop plan is still
trying to make "write that down" non-optional. We have the honesty
vocabulary. He has the injection.

**Three axes that must not be conflated, and the code treats a conflation as
a bug.** Bitemporal invalidation (`valid_from`/`valid_to`,
`created_at`/`expired_at`) is "this is false or superseded." ACT-R
activation is "this is stale." Bayesian confidence is "this is doubted." A
memory can be hot and doubted, or dormant and certain. Only contradiction
touches two axes at once, and it says so.

Clara has one axis that moves and one that does not. `half_life_days: null`
is durable. Anything else fades from `last_seen`. There is no confidence.
There is no "this is false but keep it for as-of." Replace archives a
prior body; forget deletes; consolidate archives what went cold. Those are
three different disappearances, and none of them is a point-in-time
belief. The home AGENTS.md already cares about this more than the schema
does — "an unqualified permanent fact is a claim it will still be true in
2031" — but the store cannot say "I used to believe X."

**`as_of` refuses to invent a past it cannot reconstruct.** Point-in-time
Brain recall ranks by similarity alone, reports `score_basis =
similarity_only`, writes no accesses, and stays out of transition
learning. Clara has `--at` on write so the decay clock is honest, and no
read-time as-of at all. Recall refuses to answer `as_of` from current
state and degrades coverage for sources that declare `as_of_support:
none`. Naming the scoring basis on the response is still cheap and good.

**Spreading activation exists to find the thing that shares no vocabulary
with the query.** Clara's recall is the same lexical hole the documents
adapter has, just over a much smaller file. `DoR` is careful. A paraphrase
that shares no token is a miss. Brain can surface that paraphrase because
he owns a graph over one embedding space. Recall cannot put spreading in
the core without a common index. qmd-as-a-source is the version that
survives our invariants, and the admission-floor problem it creates is
still unsolved.

**Explainability is an API contract, not a flag.** Every Brain recall
result includes the per-component breakdown. Retrievals log the frozen
`ParamVersion` and weights. `brain_explain` and UI views are access weight
0, so inspecting a memory does not make it more recallable. Clara's
`preferences explain` is the version of this that matches a conservative
store: it prints the observation ids and refs that built a generated
preference. Hand-written facts have no such ledger. Recall's `--explain`
is the diagnostic tier; pointer output is for choosing a locator.

**The LLM is not trusted with the safety property.** Shared-tier promotion
asks Sonnet to generalize, then runs a mechanical gate the model cannot
talk past. Clara never hands an LLM the write. That is a stricter answer
to the same fear, and it is why Clara cannot distill a hub of notes into a
lesson unless a human or an agent types the lesson. If Clara ever grows a
distill store, Brain's shape is the one to copy: the model proposes, a
boring check disposes.

**Near-duplicate writes fold.** Cosine ≥ 0.90 creates nothing and tells the
agent to revise. Clara folds only on exact subject. A restated preference
under a new subject is a second row until consolidate merges duplicate
subjects — and that merge keeps the first body, so the restatement is
lost. Body-clobber refusal is the better safety for a typed subject. It is
the worse fold.

**Doctor proves the lock is locked.** The invalid-token probe is the one I
would copy onto `recall serve`. Clara does not have this problem: there is
no daemon to leave unauthenticated.

**Work tracking in the same graph is a coherent product for his scope.**
`brain_precheck_file` before you repeat a failed fix, commit hooks,
`brain_where_left_off`. On this machine those records already live in td,
Tasks, and Clara signals. Putting them in a second system of record would
be the mistake. The idea worth taking is the warning, not the store.

## What Recall does better

**Honesty about coverage.** Dense retrieval always returns a ranked list. A
Brain brief that cannot reach the daemon fails open and looks like a
project with no memory. Clara memory recall that finds nothing is a true
empty of one file; it says nothing about td or the notes tree. We spend an
embarrassing amount of spec prose on the difference between "nothing
matched," "something could not answer," and "nothing searched." That prose
is load-bearing. His fail-open hooks are the right adoption trade. They
are the wrong retrieval contract.

**We do not own the corpus.** Notes stay notes. td stays td. Gmail stays
Gmail. Clara memory stays Clara memory. A source can be unhealthy, denied
by a sensitivity ceiling, or absent on this machine, and the query still
means something. Brain cannot see `docs/spec.md` unless someone remembered
a paraphrase of it. Clara memory cannot either — that is why the home
AGENTS.md says reach for `recall query` first.

**Pointers, then expand.** Returning full engram content on every Brain
recall is correct when the record is a paragraph you wrote last Tuesday.
Clara returns the whole JSONL row, which is the same bet at a smaller
scale: a memory body is supposed to be a sentence, with a path if you need
more. Recall's pointer tier exists because we measured a real query at 22
KB of which the four primaries were 3 KB.

**Query does not change the ranking.** Invariant 3 and the decay section
are explicit: retrieved content is data; retrieval hits never reinforce.
Clara already obeys this inside its own store. `memory recall` does not
bump `hits`. Reinforcement is `memory add` on the same subject, or a new
observation. Brain's whole point is the opposite, and the weights (hit
1.0, spread 0.5, brief 0.25, explain 0) are a careful attempt to stop
rich-get-richer. That is right for a memory he owns. It is wrong for a
document we do not, and Clara was right not to copy it.

**Cross-source fusion without a shared scale.** Rank fusion, declared
lineage, corroboration that does not let two views of one file count as
two witnesses. Brain has one embedding space. Clara has one JSONL file.
We have the problem on every query.

**The trust boundary is a cloned-repository problem, not a tailnet
problem.** A project `recall.toml` cannot declare an executable, cannot
repoint a user-layer source, cannot raise a sensitivity ceiling. Clara's
threat model is "the CLI is the only writer, source text is untrusted
data, never hand-edit the JSONL." Brain's is "the daemon is on the Studio,
clients come in through Caddy, RLS is the isolation." All three are
coherent. Only Recall's has to survive `git clone` onto a stranger's
laptop.

**Evaluation is a product.** `recall eval` runs through the same
application layer as the CLI. Smoke and shapes fail the build when a
metric moves down. Clara's memory tests are serious — clobber refusal,
`--at` in the future, query-required recall, decay arithmetic, preference
replay — and they answer "did this store stay honest," not "did this
change retrieve better evidence." Brain has golden math and a retrieval
log he calls a future tuning set.

**Portability and cost of being down.** Recall is a binary plus a TOML
file. Clara memory is a Ruby CLI plus two JSONL files. Brain is Postgres,
Ollama, a LaunchAgent, a Caddy site, an Anthropic key file, and a tailnet.
When Ollama is down he cannot embed a write. Clara can `memory add` on an
airplane. qmd absent is a named degraded Recall source, not a core outage.

**Sensitivity is a floor, not a vibe.** `public < internal < confidential
< restricted`. Clara's memory is `confidential` and invisible under
`home` on purpose. Brain's scopes are project / user / shared, with RLS.
Different problem. Ours is the one you need when mail, calendar, and a
work notes tree share a process.

## What Clara memory does better

**The store is a file an agent can repair.** Fixed key order, one record
per line, atomic temp→fsync→rename, an advisory lock around the full
read→modify→write. `rg`, `git diff`, and `clara check` are the debugger.
Brain's rows live behind RLS and a daemon. That is the right shape for a
multi-project service with a shared tier. It is the wrong shape for "the
cat just knocked my coffee off and I need to see what we wrote about the
dentist."

**Observations are the truth; preferences are a view.** Four unanimous
reactions in one `source + kind`, latest per ref, full provenance on the
generated row, replay that replaces only that projection, disable that
survives replay. Brain asks Sonnet whether two sentences contradict.
Clara asks whether the owner dismissed four Jira bugs and zero contrary.
The first can catch "the service listens on 8100" versus "9000." The
second cannot invent a taste the owner did not demonstrate. Clara's
learning-loop plan is explicit: distill never invents score weights.
That conservatism is the product.

**The CLI is the only writer, and the surface is complete.** `list`,
`recall`, `add`, `forget`, `consolidate`, `observe`, `preferences
list|explain|remove|restore`, every read with `--json`, `--help` that
names every flag including the defaults a new subject gets. Discovering
`memory add` used to mean triggering one required-flag error at a time;
that is a closed ticket. Brain's agent path is MCP. `brainctl` is admin.
An agent without MCP can still POST to `/api/v1`, but that is not the
path the tools teach. Clara would rather fail a session that tried to
edit the JSONL by hand than grow a second writer.

**Decay is read-time arithmetic, not a job that must run.** Brief scoring
uses today's date and current memory. Nothing in the file changes because
you looked at it. Consolidate is the only pass that archives. Brain's
hourly SQL decay is golden-tested against the Go closed form, which is
impressive and also a process you have to keep up. Clara's formula fits
in one line of the data contract.

**How-to does not live in memory.** Playbook and skills take procedure.
Memory takes what-is. Observe takes what the owner did. Brain will
happily store a gotcha as a `fix` engram and later promote it into a
shared lesson. That is powerful and it mixes the categories Clara spent
pages keeping apart. A fetch workaround that only exists as a memory row
is the thing the learning-loop document exists to stop.

**Body clobber is a refused write, not a silent fold.** `--replace`
archives. `--append` concatenates. `--dry-run` shows the would-be
refusal. Brain's near-dup fold is kinder to an agent that restates
itself and meaner to an agent that meant to correct a sentence and hit
0.91 cosine. Clara made the opposite bet, and for a store whose subjects
are typed (`person:`, `project:`, `pref:observed:`) I think it is the
right one.

**Check-ins turn a fact into a follow-up without minting a fake signal.**
A subject, sourced facts, a cadence, `needs_sources` so `clara check`
can complain that a recruiting check-in never fetches `calendar:horizon`.
Brain's subscriptions fire when a write matches an embedded context, or
when activation crosses a precomputed threshold. Different trigger.
Clara's version is the one you want for "ask Alex about the visa in two
weeks."

## What to take, and what to leave

Leave the store out of Recall. Leave ACT-R in the core. Leave Hebbian
edges. Leave the write tools. Leave work-tracking entities. Those are
Brain, or they are Clara. The spec's exclusion list is still right. A
Recall that accepted `brain_remember` would have to decide what is true,
and then it would be a worse Brain with a document index taped on.

Leave Postgres, nightly NLI, and retrieval-side-effect ranking out of
Clara. The JSONL, the observation ledger, and read-time decay are the
reasons this store is pleasant to run. Brain's cognitive model is a
better ontology than `half_life_days` plus "non-decaying," and it is
still an ontology you can steal as fields and rules without stealing the
daemon.

Take the habit, on the host side. Clara already has a brief. The thing
Brain demonstrates is that *some* memory has to arrive without the agent
asking: current decisions, recently invalidated beliefs the model may
still be treating as fact, a write nudge that names the subject to use.
Clara should not paste the whole file. A small, token-budgeted section —
hot projects, generated preferences, facts whose `last_seen` is today —
would close the "forgot to recall" hole without becoming MEMORY.md.
`clara memory recall` staying separate from `recall query` remains
correct. A SessionStart hook that calls `recall query` in `pre_reply`
is a Clara (or orchestrator) feature. It should fail open. Doctor should
be how you notice it is failing.

Take `score_basis` for Recall `as_of`. If a source cannot reconstruct
historical activation, the response should say the ranking is
similarity-only.

Take the invalid-token probe for `recall serve` / `recall doctor
--server`. A green doctor on a server that is not requiring a token is
how his plist drift bit him, and he wrote it down.

Take an `invalidate` that is not `forget`, if Clara grows a belief it
must recant. Archive-and-drop cannot answer "what did we used to think."
`--replace` already snapshots a body. Surfacing those snapshots on
purpose, with a reason, is most of the feature.

Take "inspecting is not reinforcing." Clara already does this. Keep it
when anyone proposes bumping `hits` from `memory recall` or from a
Recall expand of a Clara row.

Take fold-on-near-duplicate only if Clara ever accepts untyped prose
under fresh subjects. The current subject key makes that mostly
unnecessary. Take the secret scan that never echoes the match, on any
new remember-style MCP tool.

Take the mechanical-gate-after-the-model pattern for anything we let an
LLM propose: Clara distill, a future reranker threshold, a Brain-style
lesson. The prompt is not the safety net.

Do not take "cosine always returns k." That is the admission-floor
problem in `plans/semantic-retrieval.md`. Clara's empty table is the
honest answer for a lexical store. Brain's spreading-activation soft
gate is a clever local fix inside one graph. It does not define "nothing
matched" for a heterogeneous profile.

Do not take a web UI into Recall to keep up. If a human explorer shows
up it should be a client of `recall serve`. Brain's UI is justified
because Brain owns the graph. We own locators. Clara owns files.

A Brain adapter is the one integration that is in-shape for this repo:
an external process, JSON-RPC, his retrieval as one source among others,
his daemon outage reported as degraded coverage rather than as an empty
success. It would search the memories agents wrote on his machine. It
would not replace documents, td, mail, or Clara memory. I would only
build it if someone on this machine is actually writing those memories.

## The three jobs, kept separate

The tempting misread is Recall versus Brain. The real map is three
stores and one search layer.

Clara memory owns decaying facts, owner-stated preferences, and a
rebuildable projection of behavior. Brain owns a cognitive graph with
invalidation, association, and session injection. Recall owns neither.
It searches both, plus everything that is not a memory at all.

Clara is further along on inspectable files, a CLI that is the only
store writer, observation-backed learning that will not invent a taste,
and not needing Postgres to remember that you take your coffee a certain
way. Brain is further along on contradiction, typed decisions, session
continuity, cross-project abstraction, and making the next session see
the last one without being asked. Recall is further along on telling you
when the search was incomplete.

If we steal, we steal Brain's injection and three-axis vocabulary into
Clara's write path, and Brain's `as_of` basis and server doctor into
Recall's honesty edges. We do not steal Brain's ontology into
`internal/ranking`, and we do not steal Recall's job into either memory
store.

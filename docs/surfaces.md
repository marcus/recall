# HTTP and MCP surfaces

The CLI, HTTP API, and MCP server call one typed application contract. Running
through a service changes process lifetime and latency, not retrieval,
permissions, ranking, result shape, or error meaning.

## Local HTTP API

Start a server for one profile:

```sh
recall serve --profile work
# recall serve profile work at http://127.0.0.1:8765
```

The default address is the loopback literal `127.0.0.1:8765`. A hostname is not
used because name resolution can change the interface a process binds.
Header reads, request bodies, response writes, and idle connections all have
finite deadlines derived from `--request-timeout`. `SIGINT` and `SIGTERM`
trigger graceful HTTP shutdown with a five-second fallback bound.

Dispatch the operator commands to that long-lived core:

```sh
recall query --server http://127.0.0.1:8765 "what did we decide?"
recall expand --server http://127.0.0.1:8765 notes:decision-14
recall refresh --server http://127.0.0.1:8765 --source notes
recall sources --server http://127.0.0.1:8765
recall doctor --server http://127.0.0.1:8765
```

Every endpoint is under `/v1`:

| Method | Path | Result |
|---|---|---|
| `POST` | `/v1/query` | `recall.QueryResponse` |
| `POST` | `/v1/expand` | `recall.ExpandResponse` |
| `POST` | `/v1/refresh` | `recall.RefreshResponse` |
| `GET` | `/v1/sources` | the CLI source listing |
| `GET` | `/v1/doctor` | the CLI diagnosis |
| `GET` | `/v1/version` | build, API version, and served profile |

Query responses preserve the independent `outcome` and `coverage` fields.
Complete and abstained responses use 200, degraded responses use 206, and a
typed response in which every source failed uses 503. The 503 body is still a
query result, not a transport error: its source outcomes are the evidence that
nothing managed to search.

Every response is bounded. A request that names no `budget.response_tokens`
gets the default ceiling of 8000 — source outcomes and plan included — and a
negative value asks for an unbounded one. `budget.surface` names what the
caller will consume: unset, it is the body this endpoint sends, so an
undeclared caller is bounded by the bytes it receives. A client that renders a
projection of the body declares it — `pointer` or `explained` — and the budget
bounds that projection instead, which is what makes `recall query --server`
byte-identical to the same query in process. `tool`, and any value outside the
vocabulary, is refused here with `bad_request`: a tool result is consumed where
it is produced, and this endpoint is not producing one. MCP prices its own
results as what they deliver — structured content, text projection, and
envelope together — and ignores anything a caller says about it.

When a budget cannot afford the frame, or affording it whole would leave no
room for a single result, `source_outcomes` is replaced by `source_summary`
and the plan's source list is dropped, with both named in `omitted`. The
degraded sources are named either way. Shaping sets `truncated` and
`dropped_results`; it never changes `outcome`, because what the corpus said is
decided before anything is cut.

Refresh responses use the same severity mapping: 200 when every attempted
source refreshed, 206 when usable state remains but a source failed or stayed
degraded, and 503 when no target produced usable state. Those non-200 bodies
remain typed refresh results with per-source status and post-refresh health.
Omitting `source_id` targets every eligible checkpoint-capable source in the
served profile; naming one targets exactly that source.

Requests carrying an `Origin` header are refused, body-carrying requests must
be JSON, and the server emits no CORS headers. These checks keep a web page from
using the user's browser as a bridge into the loopback API.

### Authenticated non-loopback access

A non-loopback bind is rejected unless bearer authentication is configured.
Put the token in the environment rather than in an argument or URL:

```sh
export RECALL_API_TOKEN='a long random secret'
recall serve \
  --addr 192.0.2.10:8765 \
  --auth-token-env RECALL_API_TOKEN

recall query \
  --server http://192.0.2.10:8765 \
  --auth-token-env RECALL_API_TOKEN \
  "what did we decide?"
```

The server compares the token in constant time and authenticates every
endpoint. Use a private network or put a TLS-terminating proxy in front of the
server when the transport can be observed: bearer authentication proves who
the caller is, but plain HTTP does not hide the token or the returned evidence.
A separate server process is required for each profile; a request cannot select
a different profile from the one whose adapter pool is already open.

## MCP

`recall mcp --profile work` runs a Model Context Protocol server over stdio. It
exposes four tools:

- `recall_query` — ranked evidence pointers with locators, outcome, coverage,
  and a source summary naming any source that could not answer. Pass
  `explain: true` for the diagnostic tier: score explanations, cluster lineage,
  the per-source ledger, and the resolved plan.
- `recall_expand` — evidence behind one locator, with provenance and
  truncation.
- `recall_refresh` — refresh one source or all eligible checkpoint-capable
  sources, with typed per-source outcomes and post-refresh health.
- `recall_sources` — configured source capabilities, health, and freshness.

Tool results carry both a compact text block and `structuredContent`. The
structured content is the same JSON the CLI emits at the same tier — the
pointer projection by default, the complete serialization under `explain` —
and the text projection does not replace it. A failed query sets `isError`
while retaining what the response claims, so a host cannot mistake “nothing
searched” for “nothing matched.”

The default projects for the reason `recall query --json` does: the consumer is
a model paying context for every field, roughly 37% of a response is the
per-source ledger and plan and another 36% is cluster members re-serializing
each primary, and a model choosing which locator to expand needs neither. The
response budget is denominated in the tier the call actually returns.

`POST /v1/query` takes the same choice as a request field, and defaults the
other way: `"tier": "pointer"` projects, and an absent tier or `"complete"`
sends the whole response. `/v1` is a pinned API whose promise is that a client
reading it keeps working, so the smaller shape is opt-in there; a tool
description is read fresh by a model every session, so its default is a prompt
decision rather than a compatibility one. `tier` is what the server sends and
`budget.surface` is what the caller consumes: `recall query --server` receives
the complete body, prints pointers, and declares `structured_pointer` so it is
priced for what it renders.

MCP requests run concurrently. A `notifications/cancelled` message cancels the
matching application call and therefore the adapter work below it. Standard
output carries protocol messages only; diagnostics go to standard error and do
not include query text.

The client must send `initialize`, receive its response, and then send
`notifications/initialized` before listing or calling tools. At most 32
requests are in flight by default; saturation returns a JSON-RPC server-busy
error rather than queuing unbounded goroutines. Closing stdin or stopping the
server cancels every in-flight call immediately. Shutdown waits at most five
seconds for a non-cooperative call before returning and suppressing any late
write.

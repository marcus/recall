# Profile Example

This is a concrete, invented deployment for a small engineering team. It shows
how to inventory sources before writing configuration: what each source owns,
how fresh it can be, which identifiers remain stable, and which profile may
read it.

The names, paths, endpoints, records, and source UIDs below are synthetic.

## Inventory

### 1. Team handbook — indexed documents

**Location:** `/srv/recall-demo/handbook`

**Records:** Markdown runbooks, architecture notes, decisions, and onboarding
guides.

**Authority:** authoritative for written procedures and accepted design
decisions; not authoritative for live work state.

**Mode:** indexed, rebuilt when the document generation changes. Expansion
reads the original file and line range.

**Locator:** repository-relative path and line range, resolved against the
indexed source revision.

**Sensitivity:** `internal`.

### 2. Team tasks — live structured records

**Location:** `/srv/recall-demo/tasks`

**Records:** active and completed tasks, projects, state, dates, priority, tags,
and notes.

**Authority:** authoritative for task state.

**Mode:** live through the Tasks CLI JSON contract. Recall uses stable task IDs
as locators and does not read the store format directly.

**`as_of`:** `none`. A deadline or completion date is not revision history, so
the adapter must not claim it can reconstruct an earlier task state.

**Sensitivity:** `internal`.

### 3. API workspace — live engineering issues

**Location:** `/srv/recall-demo/services/api`

**Records:** td issues, epics, dependencies, review state, logs, and handoffs
for the example API service.

**Authority:** authoritative for engineering work in this workspace.

**Mode:** live through td's machine-readable CLI surfaces. The repository path
selects the workspace; Recall does not depend on td's private storage.

**Locator:** workspace identity plus issue ID.

**`as_of`:** `none`. A last-write timestamp is not enough to reproduce prior
revisions.

**Sensitivity:** `internal`.

### 4. Incident service — external adapter

**Location:** `https://incidents.example.com`

**Records:** current incidents, updates, owners, and resolution summaries.

**Authority:** authoritative for incident state reported by the service.

**Mode:** live through an external `recall-incidents` adapter speaking Recall's
JSON-RPC protocol on stdio.

**Locator:** the service's immutable incident ID.

**`as_of`:** `filter`, but only if the adapter manifest and service API both
guarantee a stable event-time boundary.

**Sensitivity:** `confidential`, because updates may quote customer reports.

## Worked configuration

Adapter commands belong in the trusted user layer. Project configuration may
select a trusted adapter but may not introduce a command to execute.

```toml
[adapters.incidents]
command = "recall-incidents"
freshness_modes = ["live"]

[defaults]
profile = "engineering"
budget_ms = 15000
timeout_ms = 2000
fusion_reserve_ms = 25
max_results = 20
relevance_floor = 0.10

[[sources]]
source_uid = "01EXAMPLEHANDBK0"
source_id = "handbook"
adapter = "documents"
location = "/srv/recall-demo/handbook"
location_kind = "path"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.4
record_types = ["document"]
freshness_policy = "indexed: rebuild after a repository revision changes"

[sources.settings]
extensions = [".md"]
exclude_nested_repos = true
examples_quote_queries = true

[[sources]]
source_uid = "01EXAMPLETASKS00"
source_id = "team-tasks"
adapter = "tasks"
location = "/srv/recall-demo/tasks"
location_kind = "path"
freshness_mode = "live"
sensitivity = "internal"
base_prior = 1.3
record_types = ["task"]
freshness_policy = "live: each search reads the Tasks CLI"

[[sources]]
source_uid = "01EXAMPLETDAPI00"
source_id = "api-work"
adapter = "td"
location = "/srv/recall-demo/services/api"
location_kind = "path"
freshness_mode = "live"
sensitivity = "internal"
base_prior = 1.2
record_types = ["task"]
freshness_policy = "live: each search reads td's CLI"

[[sources]]
source_uid = "01EXAMPLEINCDT00"
source_id = "incidents"
adapter = "incidents"
location = "https://incidents.example.com"
location_kind = "uri"
freshness_mode = "live"
sensitivity = "confidential"
base_prior = 1.5
record_types = ["event", "message"]
freshness_policy = "live: service response carries its observation time"

[sources.credentials]
bearer_token = { env_var = "RECALL_INCIDENTS_TOKEN" }

[profiles.engineering]
sources = ["handbook", "team-tasks", "api-work"]
max_sensitivity = "internal"

[profiles.operations]
sources = ["handbook", "team-tasks", "api-work", "incidents"]
max_sensitivity = "confidential"
```

## Why the split matters

The four surfaces share one query and ranking core, but they remain separate
sources because they make different authority and freshness claims. A handbook
sentence can explain an incident process without becoming the live incident,
and an issue can plan a task without becoming its current task state.

The confidential incident source is absent from `engineering` rather than
quietly downgraded. An operator opts into `operations`, whose ceiling says the
caller is allowed to receive that material.

This inventory is a starting point, not a ranking benchmark. Priors should move
only after evaluation over the deployment's own questions and sources.

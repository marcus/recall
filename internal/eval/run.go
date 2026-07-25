package eval

import "time"

// Status is a run's verdict. A run whose hard gates failed is invalid rather
// than merely failing: its numbers order nothing, and comparing them to a
// baseline would give a broken candidate a score.
type Status string

const (
	StatusPass    Status = "pass"
	StatusFail    Status = "fail"
	StatusInvalid Status = "invalid"
)

// GateStatus is one acceptance gate's outcome.
type GateStatus string

const (
	GatePass    GateStatus = "pass"
	GateFail    GateStatus = "fail"
	GateSkipped GateStatus = "skipped"
)

// Run is one deterministic evaluation run: what was run, on what, with what
// result. It is the record that makes a number reproducible; per-case detail
// lives beside it in cases.jsonl.
type Run struct {
	SchemaVersion int       `json:"schema_version"`
	RunID         string    `json:"run_id"`
	Status        Status    `json:"status"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`

	Pack        PackRef     `json:"pack"`
	Environment Environment `json:"environment"`
	Metrics     Report      `json:"metrics"`
	Gates       []Gate      `json:"gates,omitempty"`
}

// PackRef identifies the pack a run measured, by content and not by path. Two
// runs naming the same content hash measured the same thing.
type PackRef struct {
	PackID      string `json:"pack_id"`
	Version     string `json:"version"`
	ContentHash string `json:"content_hash"`
}

// Environment is everything outside the pack that could have changed a number.
type Environment struct {
	RecallCommit string `json:"recall_commit"`

	// Dirty records that the tree had uncommitted changes. It is recorded, not
	// hidden: a dirty run's numbers cannot be reproduced from its commit.
	Dirty bool `json:"dirty"`

	// ProfileHash is the resolved profile with secrets removed. The profile
	// itself is never written here; a run artifact is not a place for
	// credentials.
	ProfileHash string `json:"profile_hash"`

	Adapters []Component `json:"adapters,omitempty"`
	Indexes  []Component `json:"indexes,omitempty"`
	Models   []Model     `json:"models,omitempty"`

	OS          string `json:"os"`
	Arch        string `json:"arch"`
	MemoryBytes int64  `json:"memory_bytes,omitempty"`

	CachePolicy string `json:"cache_policy,omitempty"`

	// Warm says whether caches were warmed before the run.
	Warm bool `json:"warm,omitempty"`

	// NetworkAccess records whether the network was reachable. A deterministic
	// run declares false; a run that declares true and was not a live health
	// pack is invalid.
	NetworkAccess bool `json:"network_access,omitempty"`

	Seeds map[string]int64 `json:"seeds,omitempty"`
}

// Component is a versioned participant in a run: an adapter or an index.
type Component struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// Model names a model and the artifact it was. A model name alone does not
// pin behavior, so the artifact hash travels with it.
type Model struct {
	Name         string `json:"name"`
	ArtifactHash string `json:"artifact_hash,omitempty"`
}

// Gate is one acceptance gate's result. Threshold and Observed are pointers
// because a gate can be categorical — "no fixture was mutated" has no number,
// and a zero would read as one.
type Gate struct {
	Name      string     `json:"name"`
	Status    GateStatus `json:"status"`
	Threshold *float64   `json:"threshold,omitempty"`
	Observed  *float64   `json:"observed,omitempty"`
	Detail    string     `json:"detail,omitempty"`
}

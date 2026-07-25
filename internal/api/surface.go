package api

import (
	"context"

	"github.com/marcus/recall/internal/recall"
)

// Core is the application capability a surface exposes, narrowed to what a
// transport actually needs.
//
// It is an interface rather than a concrete type for a reason beyond testing:
// [Client] satisfies it too. `recall query` answered in process and `recall
// query` dispatched at a running `recall serve` then become the same call
// against two implementations of one contract, which is exactly what the local
// API is for — identical results, differing only in latency. If a transport
// reached around this interface for anything, that substitution would stop
// being total and the two paths would start to drift.
type Core interface {
	// Query searches every eligible source and fuses what comes back. The
	// response carries its own outcome and coverage; a returned error means the
	// request could not be run at all and made no claim about the corpus.
	Query(ctx context.Context, req recall.QueryRequest) (recall.QueryResponse, error)

	// Expand retrieves the evidence behind a locator, re-checking permissions.
	// A locator this core cannot serve is an error, never empty content.
	Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error)

	// Refresh updates one checkpoint-capable source, or every eligible one in
	// the served profile. The response carries semantic success even when the
	// operation itself ran but one source failed.
	Refresh(ctx context.Context, req recall.RefreshRequest) (recall.RefreshResponse, error)

	// Sources lists the configured source instances with their capabilities,
	// health, and freshness evidence.
	Sources(ctx context.Context) (Listing, error)

	// Doctor validates configuration, trust boundary, access, health,
	// freshness, identity, and lineage.
	Doctor(ctx context.Context) (Listing, error)

	// Profile names the one profile this core answers for.
	//
	// A surface serves a single profile for its whole lifetime. The profile
	// decides which sources are eligible and what sensitivity ceiling applies,
	// and it also decides which state directory the adapter pool was built
	// against — so honoring a per-request profile would mean either rebuilding
	// the pool the server exists to amortize, or answering one profile's
	// question with another profile's state. A request that names a different
	// profile is refused rather than quietly served by this one.
	Profile() string
}

// Listing is one surface answer whose payload type belongs to another package.
//
// Payload is serialized verbatim: it is whatever `recall sources --json` and
// `recall doctor --json` already emit. Declaring those shapes a second time
// here would be a second contract to keep in step with the first, and the first
// time they diverged a caller would be told two different things about one
// installation. Status is carried alongside rather than parsed back out of the
// payload, because a transport must be able to state severity without
// interpreting a structure it deliberately does not understand.
type Listing struct {
	Payload any
	Status  Status
}

// Status is the severity vocabulary both surfaces report, for listings and for
// queries alike. It is deliberately the same three-way split the CLI exit codes
// draw: something worked, something that should have worked did not, or nothing
// worked and no claim is supportable.
type Status string

// Statuses, least to most severe.
const (
	// StatusOK means the answer is complete and every part of it is trusted.
	StatusOK Status = "ok"

	// StatusDegraded means the answer was assembled from an incomplete set of
	// sources. Whatever came back is real; what is missing is unknown.
	StatusDegraded Status = "degraded"

	// StatusFailed means nothing usable came back. For a query it means every
	// source that was asked failed, so "no results" would be a claim nothing
	// supports; for a diagnosis it means a check failed outright.
	StatusFailed Status = "failed"
)

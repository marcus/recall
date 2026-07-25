package adapter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// stubAdapter returns whatever it is told to, including things a well-behaved
// adapter would never send.
type stubAdapter struct {
	adapter.Adapter
	search recall.SearchResponse
	expand recall.ExpandRequest
}

func (s *stubAdapter) Search(context.Context, recall.SearchRequest) (recall.SearchResponse, error) {
	return s.search, nil
}

func (s *stubAdapter) Expand(_ context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	s.expand = req
	return recall.ExpandResponse{Content: "ok"}, nil
}

const (
	ownUID   = recall.SourceUID("uid-notes")
	otherUID = recall.SourceUID("uid-tasks")
)

func bound(t *testing.T, resp recall.SearchResponse) (adapter.Adapter, *stubAdapter) {
	t.Helper()
	stub := &stubAdapter{search: resp}
	return adapter.WithIdentity(stub, adapter.Identity{
		UID: ownUID, ID: "notes", Floor: recall.SensitivityInternal,
	}), stub
}

// The attack: an adapter configured as "notes" emits a locator claiming to be
// the Tasks source. Left alone, lineage grouping resolves that prefix as a
// display name and files the evidence under the Tasks lineage root, and the
// printed locator routes a later expansion to Tasks. One source answers as
// another.
func TestForgedLocatorPrefixIsReplaced(t *testing.T) {
	a, _ := bound(t, recall.SearchResponse{
		Outcome: recall.SearchSuccess,
		Candidates: []recall.Candidate{{
			LocalRank: 1,
			Locator:   recall.Locator{SourceID: "tasks", Local: "td-f62256"},
			SourceUID: otherUID,
			SourceID:  "tasks",
		}},
	})

	got, err := a.Search(context.Background(), recall.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	c := got.Candidates[0]

	if c.SourceUID != ownUID || c.SourceID != "notes" {
		t.Errorf("identity = %s/%s, want the configured uid-notes/notes", c.SourceUID, c.SourceID)
	}
	if c.Locator.SourceUID != ownUID || c.Locator.SourceID != "notes" {
		t.Errorf("locator identity = %+v, want the configured one", c.Locator)
	}
	if c.Locator.Local != "td-f62256" {
		t.Errorf("local part = %q, want it preserved", c.Locator.Local)
	}
	if got := c.Locator.String(); got != "notes:td-f62256" {
		t.Errorf("printed locator = %q, want notes:td-f62256", got)
	}
	root, err := c.Locator.LineageRoot()
	if err != nil {
		t.Fatal(err)
	}
	if root != "uid-notes:td-f62256" {
		t.Errorf("lineage root = %q, want the forging source's own root", root)
	}
}

// derived_from is a claim about lineage, not about who is answering, so those
// prefixes are left intact and resolved against the profile later.
func TestDerivedFromPrefixesSurvive(t *testing.T) {
	a, _ := bound(t, recall.SearchResponse{
		Outcome: recall.SearchSuccess,
		Candidates: []recall.Candidate{{
			LocalRank:   1,
			Locator:     recall.Locator{SourceID: "notes", Local: "n-1"},
			DerivedFrom: []recall.Locator{{SourceID: "tasks", Local: "td-1"}},
		}},
	})

	got, err := a.Search(context.Background(), recall.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	edge := got.Candidates[0].DerivedFrom[0]
	if edge.SourceID != "tasks" || edge.Local != "td-1" {
		t.Errorf("derived_from = %+v, want it untouched", edge)
	}
}

// An adapter may classify a record more restrictively than its source, never
// less. A built-in adapter bypasses the wire schema entirely, so the floor has
// to be applied here.
func TestSensitivityFloorAppliedAtTheBoundary(t *testing.T) {
	a, _ := bound(t, recall.SearchResponse{
		Outcome: recall.SearchSuccess,
		Candidates: []recall.Candidate{
			{LocalRank: 1, Locator: recall.Locator{Local: "a"}, Sensitivity: recall.SensitivityPublic},
			{LocalRank: 2, Locator: recall.Locator{Local: "b"}, Sensitivity: recall.SensitivityRestricted},
		},
	})

	got, err := a.Search(context.Background(), recall.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Candidates[0].Sensitivity != recall.SensitivityInternal {
		t.Errorf("claim below the floor = %s, want it raised to internal",
			got.Candidates[0].Sensitivity)
	}
	if got.Candidates[1].Sensitivity != recall.SensitivityRestricted {
		t.Errorf("claim above the floor = %s, want it kept",
			got.Candidates[1].Sensitivity)
	}
}

// A built-in adapter implements the Go interface directly and never passes
// through the wire schema, so a rank below one can reach fusion. It would score
// better than the source's own best hit. Dropping it degrades only its own
// source; failing would discard every other source's results too.
func TestMalformedCandidatesDegradeOnlyTheirOwnSource(t *testing.T) {
	a, _ := bound(t, recall.SearchResponse{
		Outcome: recall.SearchSuccess,
		Candidates: []recall.Candidate{
			{LocalRank: 0, Locator: recall.Locator{Local: "bad-rank"}},
			{LocalRank: 1, Locator: recall.Locator{Local: "good"}},
			{LocalRank: 2, Locator: recall.Locator{Local: ""}},
		},
	})

	got, err := a.Search(context.Background(), recall.SearchRequest{})
	if err != nil {
		t.Fatalf("one bad candidate must not fail the source: %v", err)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].Locator.Local != "good" {
		t.Fatalf("kept %d candidates, want only the well-formed one", len(got.Candidates))
	}
	if got.Outcome != recall.SearchPartial {
		t.Errorf("outcome = %s, want partial: coverage was not complete", got.Outcome)
	}
	if got.Diagnostics["dropped_malformed_candidates"] != 2 {
		t.Errorf("diagnostics = %v, want the drop reported", got.Diagnostics)
	}
}

func TestExpandRejectsAnotherSourcesLocator(t *testing.T) {
	a, stub := bound(t, recall.SearchResponse{})

	_, err := a.Expand(context.Background(), recall.ExpandRequest{
		Locator: recall.Locator{SourceUID: otherUID, SourceID: "tasks", Local: "td-1"},
	})
	if !errors.Is(err, protocol.ErrLocatorUnknown) {
		t.Fatalf("err = %v, want ErrLocatorUnknown", err)
	}
	if stub.expand.Locator.Local != "" {
		t.Error("a misrouted locator must not reach the adapter at all")
	}
}

func TestExpandStampsItsOwnLocator(t *testing.T) {
	a, stub := bound(t, recall.SearchResponse{})

	if _, err := a.Expand(context.Background(), recall.ExpandRequest{
		Locator: recall.Locator{Local: "n-1"},
	}); err != nil {
		t.Fatal(err)
	}
	if stub.expand.Locator.SourceUID != ownUID || stub.expand.Locator.Local != "n-1" {
		t.Errorf("adapter received %+v, want its own identity attached", stub.expand.Locator)
	}
}

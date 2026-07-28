package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/api"
	"github.com/marcus/recall/internal/cli"
	"github.com/marcus/recall/internal/evidence"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// --budget-tokens is the flag that exists to bound a response, and it used to
// bound excerpts: 500 tokens rendered about 3,400, which was also more than
// --limit 3 rendered. These tests hold the flag to what it names, on every
// surface, measured in the same estimator the shaper spends.
//
// The measurement is the command's own stdout. Anything less would be a test of
// the cost model rather than of the response, and a cost model that agrees with
// itself is exactly the defect being fixed here.

// budgetHarness is one query with more results than any small budget can hold,
// each one expensive enough that the difference between surfaces is visible.
func budgetHarness(t *testing.T, results int) *harness {
	t.Helper()
	docs := &fake{manifest: manifest()}
	for i := range results {
		docs.candidates = append(docs.candidates, candidate(fmt.Sprintf("doc-%03d.md", i), i+1,
			func(c *recall.Candidate) {
				c.Title = fmt.Sprintf("Document %03d — a title of the length a real one has", i)
				c.Excerpt = strings.Repeat("evidence body text ", 20)
				c.ExcerptKind = recall.ExcerptMatched
			}))
	}
	return newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": {manifest: manifest()}}),
	})
}

// surfaces are the three renderings the CLI prices, named as the flags that
// select them.
var surfaces = []struct {
	name string
	args []string
	want recall.ResponseSurface
}{
	{name: "pointer", want: recall.SurfacePointer},
	{name: "explained", args: []string{"--explain"}, want: recall.SurfaceExplained},
	{name: "structured_pointer", args: []string{"--json"}, want: recall.SurfaceStructuredPointer},
	{name: "structured", args: []string{"--json", "--explain"}, want: recall.SurfaceStructured},
}

func queryTokens(t *testing.T, h *harness, args ...string) int {
	t.Helper()
	_, stdout, stderr := h.run(append([]string{"query"}, args...)...)
	if stdout == "" {
		t.Fatalf("query %v produced no output\n%s", args, stderr)
	}
	return evidence.EstimateTokens(stdout)
}

// The whole rendered response is inside the budget: the outcome line, every
// result as this surface prints it, and — behind --explain — the source ledger
// and the plan.
//
// The only exemption is the minimal floor: the outcome, the coverage, the
// degraded sources, the suppressions, and the summary that stands in for the
// diagnostics a budget could not afford. Everything else is charged, so above
// the floor the response never exceeds the budget, and falls short of it by at
// most the cost of the first result that did not fit.
func TestBudgetBoundsTheWholeRenderedResponse(t *testing.T) {
	for _, surface := range surfaces {
		t.Run(surface.name, func(t *testing.T) {
			h := budgetHarness(t, 40)
			floor := queryTokens(t, h, append([]string{"--budget-tokens", "1"}, append(surface.args, "anything")...)...)

			// The floor is what cannot be traded away, so it has to be small
			// enough that calling it a floor is honest. Two sources here; the
			// live profile has eighteen and stays inside this.
			if floor > 200 {
				t.Errorf("the minimal floor is %d tokens, which is a budget of its own", floor)
			}

			for _, budget := range []int{200, 500, 1200, 3000} {
				args := append([]string{"--budget-tokens", fmt.Sprint(budget)}, append(surface.args, "anything")...)
				got := queryTokens(t, h, args...)
				if budget < floor {
					if got > floor {
						t.Errorf("budget %d rendered %d tokens, above this surface's floor of %d", budget, got, floor)
					}
					continue
				}
				if got > budget {
					t.Errorf("budget %d rendered %d tokens", budget, got)
				}
			}
		})
	}
}

// The diagnostics summarize under budget pressure; the claim about coverage
// does not. A source that could not answer is named whether or not the ledger
// naming it survived, or a budget would be able to buy a cleaner-looking answer
// than the evidence supports.
func TestASummarizedFrameStillNamesDegradedSources(t *testing.T) {
	docs := &fake{manifest: manifest(), candidates: []recall.Candidate{candidate("a.md", 1)}}
	tasks := &fake{manifest: manifest(), searchErr: protocol.ErrSourceUnavailable}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": tasks}),
	})

	code, out, _ := h.run("query", "--budget-tokens", "1", "--explain", "anything")
	if code != cli.ExitDegraded {
		t.Errorf("exit = %d, want %d: the budget must not change what the run claims", code, cli.ExitDegraded)
	}
	contains(t, out, "degraded coverage: tasks (unreachable)",
		"the coverage claim survives a budget that removed the ledger")
	contains(t, out, "ledger omitted for the response budget",
		"an omission a reader cannot see reads as a source that was never asked")

	_, structured, _ := h.run("query", "--budget-tokens", "1", "--json", "--explain", "anything")
	var resp recall.QueryResponse
	if err := json.Unmarshal([]byte(structured), &resp); err != nil {
		t.Fatalf("--json --explain output is not a response: %v", err)
	}
	if resp.SourceOutcomes != nil || resp.SourceSummary == nil {
		t.Fatalf("the ledger was kept in a response that could not afford it: %+v", resp)
	}
	if len(resp.SourceSummary.Degraded) != 1 {
		t.Errorf("the summary lost the degraded source: %+v", resp.SourceSummary)
	}
	if !slices.Contains(resp.Omitted, recall.OmittedSourceOutcomes) ||
		!slices.Contains(resp.Omitted, recall.OmittedPlanSources) {
		t.Errorf("omitted = %v; a fact removed for budget is named, not silently absent", resp.Omitted)
	}

	// The pointer tier reaches the same claim by never charging for the ledger
	// in the first place. "omitted" stays empty here and that is the point: it
	// means a budget removed something, and on this surface nothing was removed
	// — the ledger is not part of the shape. What must survive either way is
	// the degraded source, and it does.
	_, projected, _ := h.run("query", "--budget-tokens", "1", "--json", "anything")
	pointer := parsePointerJSON(t, projected)
	if pointer.SourceSummary == nil || len(pointer.SourceSummary.Degraded) != 1 {
		t.Errorf("the projected surface lost the degraded source: %+v", pointer.SourceSummary)
	}
	if len(pointer.Omitted) != 0 {
		t.Errorf("omitted = %v; the projection is documented and identical on every "+
			"query, so naming it here would confuse a budget fact with a shape",
			pointer.Omitted)
	}
}

// The footers are inside the budget, not exempt from it. The diagnostic tier
// prints a per-source ledger and a plan that the default tier does not, so the
// same budget has to buy fewer results — which is what charging for them means.
func TestExplainFootersAreInsideTheBudget(t *testing.T) {
	h := budgetHarness(t, 40)
	_, plain, _ := h.run("query", "--budget-tokens", "3000", "anything")
	_, explained, _ := h.run("query", "--budget-tokens", "3000", "--explain", "anything")

	if want := strings.Count(plain, "\n1. "); want == 0 {
		t.Fatal("the default tier rendered no results to compare against")
	}
	if !strings.Contains(explained, "sources") || !strings.Contains(explained, "plan") {
		t.Fatal("--explain rendered no footers, so nothing was charged for them")
	}
	if got, want := results(explained), results(plain); got >= want {
		t.Errorf("the same budget bought %d results with the footers and %d without; the footers are not being charged",
			got, want)
	}
	if got := evidence.EstimateTokens(explained); got > 3000 {
		t.Errorf("the diagnostic tier rendered %d tokens against a budget of 3000", got)
	}
}

// results counts rendered result heads, which are the only lines that start at
// column zero with a rank.
func results(out string) int {
	n := 0
	for i := 1; ; i++ {
		if !strings.Contains(out, fmt.Sprintf("\n%d. ", i)) {
			return n
		}
		n++
	}
}

// A query that found nothing is the case the old budget failed hardest: it
// charged for excerpts that did not exist and printed 6.4 KB of footers anyway.
func TestZeroResultQueryRespectsTheBudget(t *testing.T) {
	for _, surface := range surfaces {
		t.Run(surface.name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				userTOML: twoSourceTOML,
				adapters: fakeAdapters(map[string]*fake{"fakedocs": {manifest: manifest()}, "faketasks": {manifest: manifest()}}),
			})
			const budget = 200
			args := append([]string{"--budget-tokens", fmt.Sprint(budget)}, append(surface.args, "anything")...)
			if got := queryTokens(t, h, args...); got > budget {
				t.Errorf("a response with no results rendered %d tokens against a budget of %d", got, budget)
			}
		})
	}
}

// The regression the flag was reported for: --budget-tokens 500 rendered more
// than --limit 3 did, so the flag naming the smaller number produced the larger
// response.
//
// The two flags do not name the same unit, and a correct budget can afford more
// than three pointers — so what has to hold is that neither response is larger
// than what was asked for: 500 tokens, or whatever three results cost when that
// is more.
func TestBudgetIsNeverLargerThanWhatItNames(t *testing.T) {
	for _, surface := range surfaces {
		t.Run(surface.name, func(t *testing.T) {
			h := budgetHarness(t, 40)
			budgeted := queryTokens(t, h, append([]string{"--budget-tokens", "500"}, append(surface.args, "anything")...)...)
			limited := queryTokens(t, h, append([]string{"--limit", "3"}, append(surface.args, "anything")...)...)

			if ceiling := max(500, limited); budgeted > ceiling {
				t.Errorf("--budget-tokens 500 rendered %d tokens; --limit 3 rendered %d", budgeted, limited)
			}
		})
	}
}

// Shaping decisions appear in evaluation runs, so the same input and budget
// must always produce the same response.
func TestShapingIsDeterministic(t *testing.T) {
	for _, surface := range surfaces {
		t.Run(surface.name, func(t *testing.T) {
			h := budgetHarness(t, 40)
			args := append([]string{"query", "--budget-tokens", "1500"}, append(surface.args, "anything")...)

			_, out, _ := h.run(args...)
			first := shapedPart(t, out)
			for range 10 {
				_, out, _ := h.run(args...)
				if again := shapedPart(t, out); again != first {
					t.Fatalf("two runs of the same query differed\n--- first ---\n%s\n--- again ---\n%s", first, again)
				}
			}
		})
	}
}

// shapedPart is the part of a response that shaping decides. The per-source
// ledger and the resolved plan are left out of the comparison: an elapsed time
// and a remaining timeout are wall-clock facts that differ between two runs for
// reasons shaping has nothing to do with.
func shapedPart(t *testing.T, out string) string {
	t.Helper()
	if strings.HasPrefix(out, "{") {
		var resp recall.QueryResponse
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatalf("--json output is not a response: %v", err)
		}
		resp.Plan, resp.SourceOutcomes, resp.Elapsed = recall.Plan{}, nil, 0
		body, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("re-encoding the response: %v", err)
		}
		return string(body)
	}
	if i := strings.Index(out, "\nsources\n"); i >= 0 {
		return out[:i]
	}
	return out
}

// `recall query` and `recall query --server` must answer identically: the
// server exists to amortize process lifetime, not to give a different answer.
// That holds only if the budget crosses the wire meaning the same thing, so the
// request the core receives has to be the same one either way — the surface the
// CLI will render included, since that is what the response is priced against.
func TestRemoteAndLocalQueriesReachTheCoreIdentically(t *testing.T) {
	for _, surface := range surfaces {
		t.Run(surface.name, func(t *testing.T) {
			for _, budget := range [][]string{nil, {"--budget-tokens", "500"}} {
				local := &recordingCore{}
				remote := &recordingCore{}
				server := httptest.NewServer(api.NewHandler(api.ServerOptions{Core: remote}))
				defer server.Close()

				args := append([]string{"query", "--profile", "work"}, budget...)
				args = append(args, surface.args...)
				args = append(args, "anything")

				runSurfaceCLI(t, local, nil, args...)
				runSurfaceCLI(t, nil, nil, append(args, "--server", server.URL)...)

				if !reflect.DeepEqual(local.last, remote.last) {
					t.Errorf("the core saw different requests\n local: %+v\nremote: %+v", local.last, remote.last)
				}
				if local.last.Budget.Surface != surface.want {
					t.Errorf("surface reached the core as %q, want %q",
						local.last.Budget.Surface, surface.want)
				}
			}
		})
	}
}

// recordingCore answers nothing and remembers what it was asked.
type recordingCore struct {
	transportCore
	last recall.QueryRequest
}

func (c *recordingCore) Query(ctx context.Context, req recall.QueryRequest) (recall.QueryResponse, error) {
	c.last = req
	return c.transportCore.Query(ctx, req)
}

// An unset budget is the common case, and it is bounded. The ceiling is the
// product surface's, not the core's: a library caller holding the struct pays
// no rendering cost and keeps everything, which is why this is asserted through
// the command rather than through the shaper.
func TestUnsetBudgetTakesTheDefaultCeiling(t *testing.T) {
	h := budgetHarness(t, 400)

	// --limit is raised on both queries because the two budgets are independent
	// and this test is about the token one. Left at the profile's result budget,
	// an unbounded token budget renders twenty results and never approaches the
	// ceiling — which would leave BOTH assertions here passing for a reason
	// that has nothing to do with the ceiling they name.
	unset := queryTokens(t, h, "--limit", "400", "anything")
	if unset > recall.DefaultResponseTokens {
		t.Errorf("an unset budget rendered %d tokens, above the default ceiling of %d",
			unset, recall.DefaultResponseTokens)
	}

	// The escape hatch is what makes the ceiling a default rather than a limit.
	unbounded := queryTokens(t, h, "--budget-tokens", "-1", "--limit", "400", "anything")
	if unbounded <= recall.DefaultResponseTokens {
		t.Fatalf("the fixture is too small to show an unbounded response: %d tokens", unbounded)
	}
}

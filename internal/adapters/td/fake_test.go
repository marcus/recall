package td_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/adapters/td"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// The ids in the recorded workspace. td mints them randomly, so they are
// spelled out here rather than derived: a test that computed the id it expects
// would be asserting against its own arithmetic instead of against a fixture.
const (
	idEpic       = "td-38ba01" // epic: "Retrieval slice: two sources fused"
	idAdapter    = "td-277316" // open P1: "Adapter interface, supervision, and pooling"
	idLineage    = "td-7f7c49" // in_progress P2: "Lineage grouping and corroboration units"
	idIndexing   = "td-9e7a3a" // open P1: "Document corpus indexing", depends on idAdapter
	idPoller     = "td-5fad1f" // closed P3 chore: "Retire the old signal poller"
	idNotPresent = "td-000000" // never minted in this workspace
)

// workspaceRoot is what the recorded workspace was configured as. It is a
// path, not a real directory: nothing in the fake tests touches the filesystem,
// and the adapter derives the workspace name from it without looking.
const workspaceRoot = "/tmp/tdfix"

// fakeCLI stands in for the td executable.
//
// Every test in this package drives the adapter through it, so the parser, the
// merge, the ranking, and every failure path run without the real binary and
// without anyone's real workspace. What the fake replays is recorded output
// from the real td (see testdata), which is what keeps "hermetic" from also
// meaning "tested against shapes we invented".
type fakeCLI struct {
	// reply maps a matcher over argv to a canned result.
	reply func(args []string) (td.Result, error)
	// pinnedReply can distinguish evidence run through a verified --work-dir.
	// Nil means pinned and ordinary commands share the same fixture.
	pinnedReply func(root string, args []string) (td.Result, error)

	mu          sync.Mutex
	calls       [][]string
	pinnedRoots []string
	ordinary    int
}

func (f *fakeCLI) Run(ctx context.Context, args ...string) (td.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.ordinary++
	f.mu.Unlock()
	return f.respond(ctx, f.reply, args)
}

func (f *fakeCLI) RunPinned(ctx context.Context, root string, args ...string) (td.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.pinnedRoots = append(f.pinnedRoots, root)
	f.mu.Unlock()
	reply := f.reply
	if f.pinnedReply != nil {
		reply = func(args []string) (td.Result, error) { return f.pinnedReply(root, args) }
	}
	return f.respond(ctx, reply, args)
}

func (f *fakeCLI) respond(ctx context.Context, reply func([]string) (td.Result, error), args []string) (td.Result, error) {
	if err := ctx.Err(); err != nil {
		return td.Result{}, err
	}
	res, err := reply(args)
	if err == nil && ctx.Err() != nil {
		// What [td.ExecRunner] does: a child killed by the deadline also exits
		// non-zero, and the deadline is the truer explanation.
		return td.Result{}, ctx.Err()
	}
	return res, err
}

// wedgedCLI never answers. It models the process that has to be killed rather
// than waited for, which is the case a declared timeout either enforces or
// does not.
type wedgedCLI struct{}

func (wedgedCLI) Run(ctx context.Context, _ ...string) (td.Result, error) {
	<-ctx.Done()
	return td.Result{}, ctx.Err()
}

func (w wedgedCLI) RunPinned(ctx context.Context, _ string, args ...string) (td.Result, error) {
	return w.Run(ctx, args...)
}

func (f *fakeCLI) invocations() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

func (f *fakeCLI) pinnedInvocations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pinnedRoots...)
}

func (f *fakeCLI) ordinaryInvocations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ordinary
}

// countCalls reports how many invocations named a subcommand.
func (f *fakeCLI) countCalls(subcommand string) int {
	n := 0
	for _, call := range f.invocations() {
		if len(call) > 0 && call[0] == subcommand {
			n++
		}
	}
	return n
}

// queries returns the free text every `td search` invocation carried. It is
// the argument after the `--` separator, which is the only place this adapter
// is allowed to put query text.
func (f *fakeCLI) queries() []string {
	var out []string
	for _, call := range f.invocations() {
		if len(call) > 0 && call[0] == "search" {
			out = append(out, call[len(call)-1])
		}
	}
	return out
}

// fixture reads a recorded td response.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// recordedWorkspace replays a real td workspace of five issues: an epic, three
// tasks under it, and a closed chore. Anything it was not given a recording
// for answers the way td does — a not_found envelope on exit 1 — rather than
// inventing a shape.
func recordedWorkspace(t *testing.T) *fakeCLI {
	t.Helper()
	listAll := fixture(t, "list_all.json")
	listOpen := fixture(t, "list_open.json")
	info := fixture(t, "info.json")
	notFound := fixture(t, "show_not_found.json")
	searches := map[string][]byte{
		"adapter":       fixture(t, "search_adapter.json"),
		"supervision":   fixture(t, "search_supervision.json"),
		"lineage":       fixture(t, "search_lineage.json"),
		"corroboration": fixture(t, "search_corroboration.json"),
	}
	shows := map[string][]byte{
		idAdapter:  fixture(t, "show_a.json"),
		idLineage:  fixture(t, "show_b.json"),
		idIndexing: fixture(t, "show_c.json"),
		idPoller:   fixture(t, "show_d.json"),
	}

	return &fakeCLI{reply: func(args []string) (td.Result, error) {
		switch args[0] {
		case "list":
			if hasArg(args, "--status=open") {
				return ok(listOpen), nil
			}
			return ok(listAll), nil
		case "info":
			return ok(info), nil
		case "search":
			if body, recorded := searches[args[len(args)-1]]; recorded {
				return ok(body), nil
			}
			return ok(fixture(t, "search_none.json")), nil
		case "show":
			if body, recorded := shows[args[1]]; recorded {
				return ok(body), nil
			}
			return td.Result{Stdout: notFound, ExitCode: 1, Elapsed: time.Millisecond}, nil
		case "depends-on":
			// Keyed by id: the indexing task waits on the adapter task, and
			// the adapter task waits on nothing. A fake that answered both the
			// same way would let an expansion attribute one issue's graph to
			// another and still pass.
			if args[1] == idIndexing {
				return ok(fixture(t, "depends_on_c.json")), nil
			}
			return ok(fixture(t, "depends_on_a.json")), nil
		case "blocked-by":
			if args[1] == idAdapter {
				return ok(fixture(t, "blocked_by_a.json")), nil
			}
			return ok(fixture(t, "blocked_by_c.json")), nil
		case "files":
			return ok(fixture(t, "files_a.json")), nil
		}
		t.Errorf("unexpected invocation: td %s", strings.Join(args, " "))
		return td.Result{}, nil
	}}
}

func ok(body []byte) td.Result {
	return td.Result{Stdout: body, Elapsed: time.Millisecond}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// fixedClock keeps observation timestamps out of assertions.
var fixedClock = func() time.Time { return time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC) }

// newAdapter builds an initialized adapter over a fake td.
func newAdapter(t *testing.T, cli td.Runner, settings map[string]any) *td.Adapter {
	t.Helper()
	a, err := initAdapter(t, cli, workspaceRoot, settings)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return a
}

// initAdapter is the handshake itself, returned unwrapped so a test can assert
// on a rejection instead of failing on one.
func initAdapter(t *testing.T, cli td.Runner, location string, settings map[string]any) (*td.Adapter, error) {
	t.Helper()
	a := td.New(td.Options{Runner: cli, Clock: fixedClock})
	if _, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            t.TempDir(),
		SourceID:           "td",
		Location:           location,
		Settings:           settings,
	}); err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, nil
}

func search(t *testing.T, a *td.Adapter, query string) (recall.SearchResponse, error) {
	t.Helper()
	return searchWith(t, a, recall.SearchRequest{Query: query, Limit: 20})
}

func searchWith(t *testing.T, a *td.Adapter, req recall.SearchRequest) (recall.SearchResponse, error) {
	t.Helper()
	if req.Deadline.IsZero() {
		req.Deadline = time.Now().Add(time.Minute)
	}
	return a.Search(context.Background(), req)
}

// ids renders a result list as the issue ids it named, in rank order, which is
// what almost every assertion in this package is really about.
func ids(resp recall.SearchResponse) []string {
	out := make([]string, 0, len(resp.Candidates))
	for _, c := range resp.Candidates {
		out = append(out, c.SourceRecordID)
	}
	return out
}

func expand(t *testing.T, a *td.Adapter, local string, detail recall.DetailLevel, budget int64) (recall.ExpandResponse, error) {
	t.Helper()
	return a.Expand(context.Background(), recall.ExpandRequest{
		Locator:  recall.Locator{SourceID: "td", Local: local},
		Detail:   detail,
		Budget:   budget,
		Deadline: time.Now().Add(time.Minute),
	})
}

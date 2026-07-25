package tasks_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/adapters/tasks"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// fakeCLI stands in for the tasks executable.
//
// Every test in this package drives the adapter through it, so the parser, the
// ranking, and every failure path run without the real binary or the real
// corpus. What the fake replays is recorded output from the real CLI (see
// testdata), which is what keeps "hermetic" from also meaning "tested against
// shapes we invented".
type fakeCLI struct {
	// reply maps a matcher over argv to a canned result.
	reply func(args []string) (tasks.Result, error)

	mu    sync.Mutex
	calls [][]string
}

func (f *fakeCLI) Run(ctx context.Context, args ...string) (tasks.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return tasks.Result{}, err
	}
	res, err := f.reply(args)
	if err == nil && ctx.Err() != nil {
		// What [tasks.ExecRunner] does: a child killed by the deadline also
		// exits non-zero, and the deadline is the truer explanation.
		return tasks.Result{}, ctx.Err()
	}
	return res, err
}

// wedgedCLI never answers. It models the process that has to be killed rather
// than waited for, which is the case a declared timeout either enforces or
// does not.
type wedgedCLI struct{}

func (wedgedCLI) Run(ctx context.Context, _ ...string) (tasks.Result, error) {
	<-ctx.Done()
	return tasks.Result{}, ctx.Err()
}

func (f *fakeCLI) invocations() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
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

// fixture reads a recorded CLI response.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// recordedStore replays the recorded example store: the full listing, the
// project rollup, per-id shows, and a body probe. Anything it was not given a
// recording for answers "no match" rather than inventing a shape.
func recordedStore(t *testing.T) *fakeCLI {
	t.Helper()
	listAll := fixture(t, "list_all.json")
	listOpen := fixture(t, "list_open.json")
	listDone := fixture(t, "list_done.json")
	projects := fixture(t, "projects.json")
	bodySite := fixture(t, "list_body_site.json")
	shows := map[string][]byte{
		"aaaa0005": fixture(t, "show_task.json"),
		"aaaa0007": fixture(t, "show_done.json"),
		"aaaa000c": fixture(t, "show_notes.json"),
	}
	notFound := fixture(t, "show_not_found.txt")

	return &fakeCLI{reply: func(args []string) (tasks.Result, error) {
		switch args[0] {
		case "list":
			switch {
			case hasArg(args, "--body") && hasArg(args, "/site"):
				return tasks.Result{Stdout: bodySite, Elapsed: time.Millisecond}, nil
			case hasArg(args, "--body"):
				return tasks.Result{Stdout: []byte("[]"), Elapsed: time.Millisecond}, nil
			case hasArg(args, "--open"):
				return tasks.Result{Stdout: listOpen, Elapsed: time.Millisecond}, nil
			case hasArg(args, "--done"):
				return tasks.Result{Stdout: listDone, Elapsed: time.Millisecond}, nil
			case hasArg(args, "--archived"):
				return tasks.Result{Stdout: []byte("[]"), Elapsed: time.Millisecond}, nil
			default:
				return tasks.Result{Stdout: listAll, Elapsed: time.Millisecond}, nil
			}
		case "projects":
			return tasks.Result{Stdout: projects, Elapsed: time.Millisecond}, nil
		case "check":
			return tasks.Result{Stdout: fixture(t, "check_ok.json"), Elapsed: time.Millisecond}, nil
		case "show":
			if body, ok := shows[args[1]]; ok {
				return tasks.Result{Stdout: body, Elapsed: time.Millisecond}, nil
			}
			return tasks.Result{Stderr: notFound, ExitCode: 2, Elapsed: time.Millisecond}, nil
		}
		t.Errorf("unexpected invocation: tasks %s", strings.Join(args, " "))
		return tasks.Result{}, nil
	}}
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
var fixedClock = func() time.Time { return time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC) }

// newAdapter builds an initialized adapter over a fake CLI.
func newAdapter(t *testing.T, cli tasks.Runner, settings map[string]any) *tasks.Adapter {
	t.Helper()
	a := tasks.New(tasks.Options{Runner: cli, Clock: fixedClock})
	if _, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            t.TempDir(),
		SourceID:           "tasks",
		Settings:           settings,
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func search(t *testing.T, a *tasks.Adapter, query string) (recall.SearchResponse, error) {
	t.Helper()
	return a.Search(context.Background(), recall.SearchRequest{
		Query:    query,
		Limit:    20,
		Deadline: time.Now().Add(time.Minute),
	})
}

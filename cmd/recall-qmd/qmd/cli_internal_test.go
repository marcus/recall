package qmd

import (
	"errors"
	"strings"
	"testing"
)

// The argv allowlist is the last gate before a process is spawned, and it admits
// whole invocation shapes rather than subcommands. These are the shapes it must
// admit and the ones it must not: `qmd cleanup`, `qmd collection remove`, `qmd
// init`, and `qmd mcp` are unreachable rather than merely undeclared.
func TestAllowedArgvShapes(t *testing.T) {
	ok := [][]string{
		{"--version"},
		{"status"},
		{"collection", "show", "fixture"},
		{"search", "--json", "-n", "10", "-c", "fixture", "--", "dental"},
		{"vsearch", "--json", "-n", "1", "-c", "fixture", "--", "dental"},
		{"query", "--json", "--explain", "-n", "25", "-c", "fixture", "--", "dental"},
		{"query", "--json", "--explain", "--no-rerank", "-n", "25", "-c", "fixture", "--", "dental"},
	}
	for _, args := range ok {
		if err := checkAllowed(args, false); err != nil {
			t.Errorf("%v was refused: %v", args, err)
		}
	}

	refused := [][]string{
		{},
		{"cleanup"},
		{"init"},
		{"mcp"},
		{"collection", "remove", "fixture"},
		{"collection", "add", "."},
		{"get", "fixture/notes/a.md"},
		{"ls", "fixture"},
		{"status", "--format", "json"},
		{"search", "--json", "-n", "10", "-c", "fixture", "dental"},       // no separator
		{"search", "--json", "-n", "10", "-c", "fixture", "--", "a", "b"}, // two operands
		{"search", "--json", "-n", "10", "-c", "../elsewhere", "--", "a"}, // not a collection name
		{"search", "--json", "-n", "0x10", "-c", "fixture", "--", "a"},    // not a count
		{"search", "--json", "-n", "10", "-c", "fixture", "--", ""},       // empty query
		{"query", "--json", "-n", "10", "-c", "fixture", "--", "a"},       // explain is not optional
		{"query", "--json", "--explain", "--no-rerank", "-n", "10", "-c", "fixture", "--", "a", "--full"},
	}
	for _, args := range refused {
		err := checkAllowed(args, false)
		if err == nil {
			t.Errorf("%v was admitted", args)
			continue
		}
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%v gave %v, want ErrNotAllowed", args, err)
		}
	}
}

// The index-mutating shapes exist, and they are reachable only from a refresh.
// A search that could rebuild an index as a side effect of a query is the defect
// this split prevents.
func TestMaintenanceShapesNeedMaintenanceIntent(t *testing.T) {
	for _, args := range [][]string{{"update"}, {"embed", "-c", "fixture"}} {
		if err := checkAllowed(args, true); err != nil {
			t.Errorf("refresh cannot run %v: %v", args, err)
		}
		err := checkAllowed(args, false)
		if err == nil {
			t.Errorf("%v was reachable from a query", args)
			continue
		}
		if !strings.Contains(err.Error(), "refresh") {
			t.Errorf("%v gave %v, want the reason named", args, err)
		}
	}
}

// qmd's exit status carries no outcome semantics, so every classification is
// made from the output. None of these may become an empty success.
func TestDecodeResultsSeparatesFailureModes(t *testing.T) {
	if hits, err := decodeResults(Result{Stdout: []byte("[]\n")}, "search"); err != nil || len(hits) != 0 {
		t.Fatalf("`[]` must be an empty success: %v %v", hits, err)
	}
	if _, err := decodeResults(Result{Stdout: []byte("")}, "search"); !errors.Is(err, errBrokenContract) {
		t.Errorf("empty stdout gave %v, want a broken contract", err)
	}
	if _, err := decodeResults(Result{Stdout: []byte("Downloading model 12%\n")}, "query"); !errors.Is(err, errBrokenContract) {
		t.Errorf("progress output gave %v, want a broken contract", err)
	}
	if _, err := decodeResults(Result{Stdout: []byte(`{"error":"nope"}`)}, "query"); !errors.Is(err, errBrokenContract) {
		t.Errorf("a JSON object gave %v, want a broken contract", err)
	}
	if _, err := decodeResults(Result{Stdout: []byte(`[{"docid":`)}, "query"); !errors.Is(err, errBrokenContract) {
		t.Errorf("truncated JSON gave %v, want a broken contract", err)
	}
	err := decodeResultsErr(t, Result{ExitCode: 1, Stderr: []byte("SqliteError: locked\n")})
	if errors.Is(err, errBrokenContract) {
		t.Errorf("a non-zero exit is unreachable, not a broken contract: %v", err)
	}
	if !strings.Contains(err.Error(), "SqliteError") {
		t.Errorf("the reason was lost: %v", err)
	}
}

func decodeResultsErr(t *testing.T, res Result) error {
	t.Helper()
	_, err := decodeResults(res, "query")
	if err == nil {
		t.Fatal("expected a failure")
	}
	return err
}

// Diagnostics are read by a person in a terminal, and qmd colorizes and animates
// its output.
func TestScrubRemovesTerminalControl(t *testing.T) {
	got := scrub("\x1b[31mERROR\x1b[0m: \rmodel missing\x07")
	if got != "ERROR: model missing" {
		t.Fatalf("scrub = %q", got)
	}
	if safeDetail(nil, nil) != "no output" {
		t.Fatal("safeDetail invented output")
	}
	long := strings.Repeat("x", safeDetailLimit*2)
	if quoted := safeDetail([]byte(long)); len(quoted) > safeDetailLimit+4 {
		t.Fatalf("safeDetail returned %d bytes", len(quoted))
	}
	// A multi-byte rune must not be cut in half on the way into a diagnostic.
	wide := strings.Repeat("é", safeDetailLimit)
	if quoted := safeDetail([]byte(wide)); !strings.HasSuffix(quoted, "…") {
		t.Fatalf("safeDetail = %q", quoted)
	}
}

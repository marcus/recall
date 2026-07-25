package tasks_test

import (
	"testing"

	"github.com/marcus/recall/internal/adapters/tasks"
	"github.com/marcus/recall/internal/recall"
)

// The id this table is built around. It is a real id from the recorded example
// store, so the false positives below are the shapes a real query produces.
const knownID = "aaaa0005"

// TestExactIdentifierAtTokenBoundaries is the contract from
// docs/adapter-protocol.md: exact_identifier is emitted only for an exact
// match on a stable identifier, at token boundaries, and an unbounded
// substring match never carries it.
//
// The false-positive half of this table matters more than the true-positive
// half. exact_identifier is a partition in fusion, not a score bonus — a
// cluster carrying it sorts above every cluster that does not — so a signal
// emitted for a string that merely contains an id promotes the wrong record
// past every correct one, and no amount of downstream scoring can undo it.
func TestExactIdentifierAtTokenBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		query string
		exact bool
		why   string
	}{
		{
			name: "bare id", query: knownID, exact: true,
			why: "the whole query is the id",
		},
		{
			name: "id in a sentence", query: "what is the state of " + knownID + "?", exact: true,
			why: "trailing sentence punctuation is not part of the token",
		},
		{
			name: "id in parentheses", query: "the vendor task (" + knownID + ") is due", exact: true,
			why: "brackets delimit, they do not join",
		},
		{
			name: "id after a reference sigil", query: "see #" + knownID, exact: true,
			why: "a leading sigil is a reference marker, not part of the identifier",
		},

		{
			name: "id inside a longer word", query: "task" + knownID + "notes", exact: false,
			why: "no boundary on either side; this is the unbounded substring match the spec forbids",
		},
		{
			name: "id inside a URL path", query: "https://example.test/tickets/" + knownID, exact: false,
			why: "a path segment names a resource in another system that happens to share the text",
		},
		{
			name: "id inside a URL fragment", query: "https://example.test/board#" + knownID, exact: false,
			why: "the sigil is interior here, so trimming must not reach it",
		},
		{
			name: "id inside a namespaced id", query: "td-" + knownID, exact: false,
			why: "a hyphen joins; this names an identifier in a different scheme",
		},
		{
			name: "id inside a longer id", query: knownID + "aaaa0006", exact: false,
			why: "sixteen hex digits are one token, and it is not this task",
		},
		{
			name: "id joined by an underscore", query: "backup_" + knownID, exact: false,
			why: "underscores join words, so the id is not at a boundary",
		},
		{
			name: "uppercase id", query: "AAAA0005", exact: false,
			why: "the CLI mints lowercase; two spellings would make exact_identifier fuzzy",
		},
		{
			name: "mixed-case id", query: "aaaA0005", exact: false,
			why: "same reason, and `tasks show` would happily resolve it, which is the trap",
		},
		{
			name: "truncated id", query: "aaaa000", exact: false,
			why: "seven digits is a prefix, and a prefix is not an identity",
		},
		{
			name: "over-long id", query: "aaaa00051", exact: false,
			why: "nine digits is a different token",
		},
		{
			name: "non-hex lookalike", query: "aaaa000z", exact: false,
			why: "the shape matches but the alphabet does not",
		},
		{
			name: "id-shaped word that names no task", query: "deadbeef", exact: false,
			why: "shape alone is not existence; the CLI must confirm the record",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cli := recordedStore(t)
			a := newAdapter(t, cli, nil)

			resp, err := search(t, a, tc.query)
			if err != nil {
				t.Fatalf("search: %v", err)
			}

			var found bool
			for _, c := range resp.Candidates {
				if c.Exact() {
					found = true
					if c.SourceRecordID != knownID {
						t.Errorf("exact_identifier on %s, want only %s", c.SourceRecordID, knownID)
					}
				}
			}
			if found != tc.exact {
				t.Errorf("exact_identifier emitted = %v, want %v (%s)", found, tc.exact, tc.why)
			}
		})
	}
}

// TestExactIdentifierRanksFirst pins the ordering promise: an exact id hit is
// rank 1 even when a lexically stronger candidate exists, because the core
// partitions on the signal rather than scoring it.
func TestExactIdentifierRanksFirst(t *testing.T) {
	cli := recordedStore(t)
	a := newAdapter(t, cli, nil)

	// "site" matches two other tasks lexically, in the title and in the body.
	resp, err := search(t, a, "site "+knownID)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Candidates) < 2 {
		t.Fatalf("want the exact hit plus lexical matches, got %d candidates", len(resp.Candidates))
	}

	first := resp.Candidates[0]
	if first.SourceRecordID != knownID {
		t.Errorf("rank 1 is %s, want the exactly named task %s", first.SourceRecordID, knownID)
	}
	if first.LocalRank != 1 {
		t.Errorf("local_rank = %d, want 1", first.LocalRank)
	}
	if !first.HasSignal(recall.MatchExactIdentifier) {
		t.Errorf("rank 1 signals = %v, want exact_identifier", first.MatchSignals)
	}
	for _, c := range resp.Candidates[1:] {
		if c.Exact() {
			t.Errorf("%s also claims exact_identifier; only the named id may", c.SourceRecordID)
		}
	}
}

// TestExactIdentifierReachesOutsideScope covers the reason an id lookup gets
// its own CLI probe: a source scoped to open work must still answer a direct
// question about a closed task, because naming an id is asking about that
// record, not about the open list.
func TestExactIdentifierReachesOutsideScope(t *testing.T) {
	const closedID = "aaaa0007" // DONE, so absent from the open listing.

	cli := recordedStore(t)
	a := newAdapter(t, cli, map[string]any{"scope": "open"})

	resp, err := search(t, a, "what happened to "+closedID)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Candidates) == 0 {
		t.Fatal("no candidates; the closed task should have been resolved by id")
	}
	if got := resp.Candidates[0].SourceRecordID; got != closedID {
		t.Fatalf("rank 1 = %s, want %s", got, closedID)
	}
	if !resp.Candidates[0].Exact() {
		t.Error("the resolved record carries no exact_identifier signal")
	}
	if cli.countCalls("show") != 1 {
		t.Errorf("show invocations = %d, want exactly 1: the corpus answers the rest",
			cli.countCalls("show"))
	}
}

// TestExactIdentifierIgnoresFuzzyResolution is the trap `tasks show` sets: it
// resolves a title substring, a line reference, and a case-insensitive id, and
// answers 0 for all of them. Only a record whose id equals the token byte for
// byte may carry the signal.
func TestExactIdentifierIgnoresFuzzyResolution(t *testing.T) {
	cli := recordedStore(t)
	// Answer every show with a real record regardless of what was asked, which
	// is what fuzzy resolution looks like from this side of the pipe.
	inner := cli.reply
	cli.reply = func(args []string) (tasks.Result, error) {
		if args[0] == "show" {
			return tasks.Result{Stdout: fixture(t, "show_task.json")}, nil
		}
		return inner(args)
	}

	a := newAdapter(t, cli, map[string]any{"scope": "open"})
	// "deadbeef" is id-shaped and names nothing, so it reaches the probe; the
	// fake answers with a different task's record.
	resp, err := search(t, a, "deadbeef")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, c := range resp.Candidates {
		if c.Exact() {
			t.Errorf("%s carries exact_identifier for a query that named a different id",
				c.SourceRecordID)
		}
	}
}

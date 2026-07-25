package conformance_test

import (
	"strings"
	"testing"

	"github.com/marcus/recall/internal/conformance"
)

func TestCompare(t *testing.T) {
	// Each case is one way a replayed frame can depart from its recording, and
	// the pointer the report has to name for it. The pointer is the whole point:
	// a conformance failure is almost always one field, and a report that
	// printed two 4KB frames and left the reader to find it is why nobody would
	// run the suite.
	tests := []struct {
		name     string
		want     string
		got      string
		volatile []string
		pointers []string
		detail   string
	}{
		{
			name: "identical frames",
			want: `{"jsonrpc":"2.0","id":1,"result":{"status":"healthy"}}`,
			got:  `{"jsonrpc":"2.0","id":1,"result":{"status":"healthy"}}`,
		},
		{
			name:     "a scalar volatile pointer masks both sides",
			want:     `{"id":1,"result":{"status":"healthy","checked_at":"2026-07-25T04:17:36.8715Z"}}`,
			got:      `{"id":1,"result":{"status":"healthy","checked_at":"2027-01-02T09:00:00.0001Z"}}`,
			volatile: []string{"/result/checked_at"},
		},
		{
			name: "an undeclared timestamp still fails",
			want: `{"id":1,"result":{"checked_at":"2026-07-25T04:17:36.8715Z"}}`,
			got:  `{"id":1,"result":{"checked_at":"2027-01-02T09:00:00.0001Z"}}`,
			// Nothing is ignored by default. Declaring a field volatile is a
			// claim about the adapter, and the suite only honors the ones made.
			pointers: []string{"/result/checked_at"},
		},
		{
			name:     "a wildcard covers every element of an array",
			want:     `{"result":{"candidates":[{"locator":"a","confirmed_at":"t1"},{"locator":"b","confirmed_at":"t2"}]}}`,
			got:      `{"result":{"candidates":[{"locator":"a","confirmed_at":"t9"},{"locator":"b","confirmed_at":"t8"}]}}`,
			volatile: []string{"/result/candidates/*/confirmed_at"},
		},
		{
			name:     "a wildcard does not excuse the fields beside it",
			want:     `{"result":{"candidates":[{"locator":"a","confirmed_at":"t1"},{"locator":"b","confirmed_at":"t2"}]}}`,
			got:      `{"result":{"candidates":[{"locator":"a","confirmed_at":"t9"},{"locator":"MOVED","confirmed_at":"t8"}]}}`,
			volatile: []string{"/result/candidates/*/confirmed_at"},
			pointers: []string{"/result/candidates/1/locator"},
		},
		{
			name:     "a wildcard covers every member of an object",
			want:     `{"result":{"timings":{"search":11,"expand":4}}}`,
			got:      `{"result":{"timings":{"search":97,"expand":0}}}`,
			volatile: []string{"/result/timings/*"},
		},
		{
			// Masking replaces a value on both sides rather than skipping the
			// comparison, so declaring a field volatile excuses its
			// unpredictability and never its absence.
			name:     "a volatile field the adapter stopped sending still fails",
			want:     `{"result":{"status":"healthy","checked_at":"t1"}}`,
			got:      `{"result":{"status":"healthy"}}`,
			volatile: []string{"/result/checked_at"},
			pointers: []string{"/result/checked_at"},
			detail:   "member missing",
		},
		{
			name:     "a pointer that matches nothing is not an error",
			want:     `{"result":{"status":"healthy"}}`,
			got:      `{"result":{"status":"healthy"}}`,
			volatile: []string{"/result/candidates/*/confirmed_at", "/error/data"},
		},
		{
			name:     "an unexpected member is named",
			want:     `{"result":{"status":"healthy"}}`,
			got:      `{"result":{"status":"healthy","debug":true}}`,
			pointers: []string{"/result/debug"},
			detail:   "not in the recording",
		},
		{
			name:     "a shorter candidate list is named at the array",
			want:     `{"result":{"candidates":[{"locator":"a"},{"locator":"b"}]}}`,
			got:      `{"result":{"candidates":[{"locator":"a"}]}}`,
			pointers: []string{"/result/candidates"},
			detail:   "length differs",
		},
		{
			name:     "a member whose type changed is named",
			want:     `{"result":{"candidates":[]}}`,
			got:      `{"result":{"candidates":{}}}`,
			pointers: []string{"/result/candidates"},
			detail:   "type differs",
		},
		{
			// An adapter in another language should not fail a replay over how
			// its encoder prints a number.
			name: "number spelling does not matter",
			want: `{"result":{"local_score":1,"offset":698}}`,
			got:  `{"result":{"local_score":1.0,"offset":6.98e2}}`,
		},
		{
			name:     "a changed number is named",
			want:     `{"result":{"local_rank":1}}`,
			got:      `{"result":{"local_rank":2}}`,
			pointers: []string{"/result/local_rank"},
		},
		{
			name:     "a member name containing a slash is escaped in the pointer",
			want:     `{"result":{"files":{"a/b":1}}}`,
			got:      `{"result":{"files":{"a/b":2}}}`,
			pointers: []string{"/result/files/a~1b"},
		},
		{
			name:     "a frame that is not JSON is reported, not panicked over",
			want:     `{"result":{}}`,
			got:      `not a frame`,
			pointers: []string{""},
			detail:   "not JSON",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			diffs := conformance.Compare("synthetic", [][]byte{[]byte(tc.want)}, [][]byte{[]byte(tc.got)}, tc.volatile)
			if len(diffs) != len(tc.pointers) {
				t.Fatalf("got %d differences, want %d:\n%s", len(diffs), len(tc.pointers), render(diffs))
			}
			for i, d := range diffs {
				if d.Pointer != tc.pointers[i] {
					t.Errorf("difference %d pointer = %q, want %q", i, d.Pointer, tc.pointers[i])
				}
				if d.Case != "synthetic" {
					t.Errorf("difference %d names case %q", i, d.Case)
				}
				if d.Response != 1 {
					t.Errorf("difference %d names response %d, want 1", i, d.Response)
				}
				if tc.detail != "" && !strings.Contains(d.Detail, tc.detail) {
					t.Errorf("difference %d detail = %q, want it to mention %q", i, d.Detail, tc.detail)
				}
			}
		})
	}
}

func TestCompareReportsMissingAndExtraFrames(t *testing.T) {
	want := [][]byte{[]byte(`{"id":1,"result":{}}`), []byte(`{"id":2,"result":{}}`)}
	got := [][]byte{[]byte(`{"id":1,"result":{}}`)}

	short := conformance.Compare("synthetic", want, got, nil)
	if len(short) != 1 || short[0].Response != 2 || !strings.Contains(short[0].Detail, "stopped") {
		t.Fatalf("short replay: %s", render(short))
	}
	long := conformance.Compare("synthetic", got, want, nil)
	if len(long) != 1 || long[0].Response != 2 || !strings.Contains(long[0].Detail, "unexpected frame") {
		t.Fatalf("long replay: %s", render(long))
	}
}

func TestCompareReportsABadVolatileDeclaration(t *testing.T) {
	// A manifest can be wrong too, and a pointer nobody can parse must not
	// silently mask nothing.
	diffs := conformance.Compare("synthetic", nil, nil, []string{"result/checked_at"})
	if len(diffs) != 1 || !strings.Contains(diffs[0].Detail, "volatile") {
		t.Fatalf("diffs = %s", render(diffs))
	}
}

func TestDifferenceStringNamesCasePointerAndValues(t *testing.T) {
	diffs := conformance.Compare("handshake",
		[][]byte{[]byte(`{"result":{"adapter_id":"recall-stream/1"}}`)},
		[][]byte{[]byte(`{"result":{"adapter_id":"recall-stream/9"}}`)}, nil)
	if len(diffs) != 1 {
		t.Fatalf("diffs = %s", render(diffs))
	}
	line := diffs[0].String()
	for _, part := range []string{"handshake", "response 1", "/result/adapter_id", `"recall-stream/1"`, `"recall-stream/9"`} {
		if !strings.Contains(line, part) {
			t.Errorf("%q does not contain %q", line, part)
		}
	}
}

func render(diffs []conformance.Difference) string {
	lines := make([]string, len(diffs))
	for i, d := range diffs {
		lines[i] = "  " + d.String()
	}
	return strings.Join(lines, "\n")
}

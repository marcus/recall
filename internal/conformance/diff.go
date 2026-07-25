package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// volatileMask is what a declared-volatile value is replaced by on both sides.
// It is a value, not a deletion, so a field that disappeared from one side
// still differs from one that is merely unpredictable.
const volatileMask = "<volatile>"

// previewBytes bounds how much of a value a difference quotes. A whole
// candidate list rendered into one line is not a readable diff.
const previewBytes = 200

// Difference is one place a replayed frame departed from the recording.
//
// Pointer names the value, not the frame: a conformance failure is almost
// always one field, and a report that printed two 4KB frames and left the
// reader to spot it would be why nobody runs the suite.
type Difference struct {
	// Case is the case directory name.
	Case string

	// Response is the 1-based index of the frame, or 0 for a statement about
	// the exchange as a whole.
	Response int

	// Pointer is an RFC 6901 JSON Pointer from the root of the frame. It is
	// empty when the difference is about the frame itself rather than a value
	// inside it.
	Pointer string

	// Want and Got are the recorded and replayed values, JSON-encoded and
	// truncated for reading. Both are empty when Detail says everything.
	Want string
	Got  string

	// Detail explains a difference that is not a plain value mismatch: a
	// missing member, an unexpected one, a frame that never arrived.
	Detail string
}

func (d Difference) String() string {
	var b strings.Builder
	if d.Case != "" {
		fmt.Fprintf(&b, "case %s: ", d.Case)
	}
	if d.Response > 0 {
		fmt.Fprintf(&b, "response %d: ", d.Response)
	}
	if d.Pointer != "" {
		fmt.Fprintf(&b, "%s: ", d.Pointer)
	}
	if d.Detail != "" {
		b.WriteString(d.Detail)
	}
	switch {
	case d.Want != "" && d.Got != "":
		if d.Detail != "" {
			b.WriteString(": ")
		}
		fmt.Fprintf(&b, "want %s, got %s", d.Want, d.Got)
	case d.Want != "":
		fmt.Fprintf(&b, " (recorded %s)", d.Want)
	case d.Got != "":
		fmt.Fprintf(&b, " (replayed %s)", d.Got)
	}
	return b.String()
}

// Compare masks the declared-volatile fields on both sides and reports every
// place the replayed frames departed from the recorded ones.
//
// Masking is applied to both sides rather than skipping the comparison, so a
// declaration covers a value's unpredictability without excusing its absence: a
// volatile /result/checked_at that the adapter stopped sending still fails.
func Compare(name string, want, got [][]byte, volatile []string) []Difference {
	var out []Difference

	pointers := make([]pointer, 0, len(volatile))
	for _, raw := range volatile {
		p, err := parsePointer(raw)
		if err != nil {
			out = append(out, Difference{Case: name, Detail: "volatile " + err.Error()})
			continue
		}
		pointers = append(pointers, p)
	}

	for i := 0; i < len(want) || i < len(got); i++ {
		switch {
		case i >= len(got):
			out = append(out, Difference{
				Case: name, Response: i + 1,
				Detail: "no frame: the replay stopped here",
				Want:   preview(want[i]),
			})
			continue
		case i >= len(want):
			out = append(out, Difference{
				Case: name, Response: i + 1,
				Detail: "unexpected frame: the recording ends here",
				Got:    preview(got[i]),
			})
			continue
		}

		wantValue, err := decodeFrame(want[i], pointers)
		if err != nil {
			out = append(out, Difference{Case: name, Response: i + 1, Detail: "recorded " + err.Error()})
			continue
		}
		gotValue, err := decodeFrame(got[i], pointers)
		if err != nil {
			out = append(out, Difference{
				Case: name, Response: i + 1,
				Detail: "replayed " + err.Error(),
				Got:    preview(got[i]),
			})
			continue
		}
		out = diffValue(out, name, i+1, "", wantValue, gotValue)
	}
	return out
}

// decodeFrame parses one frame and masks its volatile fields.
//
// Numbers are kept as [json.Number] rather than float64: an id or a byte offset
// past float64's exact range would otherwise compare equal to a value it is not.
func decodeFrame(line []byte, volatile []pointer) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("frame is not JSON: %w", err)
	}
	for _, p := range volatile {
		mask(v, p)
	}
	return v, nil
}

// diffValue walks two decoded frames in parallel, naming the pointer of every
// value that differs.
func diffValue(out []Difference, name string, response int, ptr string, want, got any) []Difference {
	report := func(detail string) []Difference {
		return append(out, Difference{
			Case: name, Response: response, Pointer: ptr,
			Want: previewValue(want), Got: previewValue(got), Detail: detail,
		})
	}

	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return report("type differs")
		}
		keys := make([]string, 0, len(w))
		for key := range w {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child, present := g[key]
			if !present {
				out = append(out, Difference{
					Case: name, Response: response, Pointer: ptr + "/" + escapeToken(key),
					Want: previewValue(w[key]), Detail: "member missing from the replayed frame",
				})
				continue
			}
			out = diffValue(out, name, response, ptr+"/"+escapeToken(key), w[key], child)
		}
		extra := make([]string, 0)
		for key := range g {
			if _, present := w[key]; !present {
				extra = append(extra, key)
			}
		}
		sort.Strings(extra)
		for _, key := range extra {
			out = append(out, Difference{
				Case: name, Response: response, Pointer: ptr + "/" + escapeToken(key),
				Got: previewValue(g[key]), Detail: "member is not in the recording",
			})
		}
		return out

	case []any:
		g, ok := got.([]any)
		if !ok {
			return report("type differs")
		}
		if len(w) != len(g) {
			out = append(out, Difference{
				Case: name, Response: response, Pointer: ptr,
				Detail: fmt.Sprintf("length differs: recorded %d, replayed %d", len(w), len(g)),
			})
		}
		for i := 0; i < len(w) && i < len(g); i++ {
			out = diffValue(out, name, response, ptr+"/"+strconv.Itoa(i), w[i], g[i])
		}
		return out

	case json.Number:
		g, ok := got.(json.Number)
		if !ok {
			return report("type differs")
		}
		if !sameNumber(w, g) {
			return report("")
		}
		return out

	default:
		if want != got {
			return report("")
		}
		return out
	}
}

// sameNumber compares two JSON numbers by value, not by spelling. 1 and 1.0 are
// the same number, and an adapter written in another language should not fail a
// replay over how its encoder prints one.
func sameNumber(a, b json.Number) bool {
	if a == b {
		return true
	}
	if ai, err := a.Int64(); err == nil {
		if bi, err := b.Int64(); err == nil {
			return ai == bi
		}
	}
	af, aerr := a.Float64()
	bf, berr := b.Float64()
	return aerr == nil && berr == nil && af == bf
}

// pointer is a parsed RFC 6901 JSON Pointer, with "*" as the wildcard the
// transcript format adds.
type pointer []string

func parsePointer(s string) (pointer, error) {
	if s == "" {
		return nil, fmt.Errorf("pointer is empty")
	}
	if !strings.HasPrefix(s, "/") {
		return nil, fmt.Errorf("pointer %q must begin with %q", s, "/")
	}
	tokens := strings.Split(s[1:], "/")
	out := make(pointer, len(tokens))
	for i, token := range tokens {
		// RFC 6901 order: ~1 before ~0, so an escaped tilde cannot become a
		// separator.
		out[i] = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
	}
	return out, nil
}

func escapeToken(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// mask replaces what the pointer reaches with [volatileMask]. "*" matches every
// element of an array and every member of an object, which is what lets one
// declaration cover a whole candidate list.
//
// A pointer that matches nothing is not an error: one declaration list is shared
// across the responses of a case, and those have different shapes.
func mask(v any, p pointer) {
	if len(p) == 0 {
		return
	}
	head, rest := p[0], p[1:]
	switch node := v.(type) {
	case map[string]any:
		if head == "*" {
			for key := range node {
				maskMember(node, key, rest)
			}
			return
		}
		if _, present := node[head]; present {
			maskMember(node, head, rest)
		}
	case []any:
		if head == "*" {
			for _, item := range node {
				mask(item, rest)
			}
			return
		}
		i, err := strconv.Atoi(head)
		if err != nil || i < 0 || i >= len(node) {
			return
		}
		if len(rest) == 0 {
			node[i] = volatileMask
			return
		}
		mask(node[i], rest)
	}
}

func maskMember(node map[string]any, key string, rest pointer) {
	if len(rest) == 0 {
		node[key] = volatileMask
		return
	}
	mask(node[key], rest)
}

func preview(raw []byte) string { return truncate(string(raw)) }

func previewValue(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return truncate(fmt.Sprint(v))
	}
	return truncate(string(encoded))
}

func truncate(s string) string {
	if len(s) <= previewBytes {
		return s
	}
	return s[:previewBytes] + "…"
}

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// out accumulates human output. Every write goes through one place, so the
// error a writer can return is handled once instead of at every call site, and
// a command can decide its exit code before any of its output is emitted.
type out struct{ buf bytes.Buffer }

func (o *out) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(&o.buf, format, args...)
}

func (o *out) line(s string) { o.printf("%s\n", s) }

func (o *out) blank() { o.printf("\n") }

// block writes indented text, leaving blank lines blank so an indented excerpt
// does not grow trailing whitespace.
func (o *out) block(indent, text string) {
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if strings.TrimSpace(l) == "" {
			o.blank()
			continue
		}
		o.printf("%s%s\n", indent, l)
	}
}

func (o *out) flush(w io.Writer) error {
	_, err := w.Write(o.buf.Bytes())
	return err
}

// emitJSON writes the machine-readable form of a command's result.
func emitJSON(w io.Writer, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	_, err = w.Write(body)
	return err
}

// fields is a labeled run of values on one line.
//
// A field whose value is zero is left out. That is the same rule
// internal/explain applies, and it is what makes human and JSON output
// comparable: a zero is the absence of a fact, not a fact, so its absence from
// the text is not a missing field.
type fields []string

func (f *fields) text(label, value string) {
	if value != "" {
		*f = append(*f, label+" "+value)
	}
}

func (f *fields) count(label string, n int) {
	if n != 0 {
		*f = append(*f, label+" "+strconv.Itoa(n))
	}
}

func (f *fields) count64(label string, n int64) {
	if n != 0 {
		*f = append(*f, label+" "+strconv.FormatInt(n, 10))
	}
}

func (f *fields) number(label string, v float64) {
	if v != 0 {
		*f = append(*f, label+" "+num(v))
	}
}

func (f *fields) dur(label string, d time.Duration) {
	if d != 0 {
		*f = append(*f, label+" "+duration(d))
	}
}

func (f *fields) at(label string, t *time.Time) {
	if t != nil {
		*f = append(*f, label+" "+stamp(*t))
	}
}

// raw appends a value that is its own label, such as "eligible".
func (f *fields) raw(value string) {
	if value != "" {
		*f = append(*f, value)
	}
}

func (f *fields) flag(label string, on bool) {
	if on {
		*f = append(*f, label)
	}
}

func (f fields) String() string { return strings.Join(f, "  ") }

func (f fields) empty() bool { return len(f) == 0 }

func num(v float64) string { return strconv.FormatFloat(v, 'g', 6, 64) }

// duration is rounded to the millisecond. Sub-millisecond precision in a
// report a person reads is noise, and it would make two identical runs differ.
func duration(d time.Duration) string { return d.Round(time.Millisecond).String() }

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// diagnostics renders an adapter's diagnostic map deterministically. It is
// source-influenced text, already sanitized upstream, and it is shown because a
// failure a person cannot see the detail of is a failure they cannot fix.
func diagnostics(in map[string]any) string {
	if len(in) == 0 {
		return ""
	}
	parts := make([]string, 0, len(in))
	for _, k := range slices.Sorted(maps.Keys(in)) {
		parts = append(parts, fmt.Sprintf("%s=%v", k, in[k]))
	}
	return strings.Join(parts, ", ")
}

func strv[T ~string](in []T) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

func join[T ~string](in []T) string { return strings.Join(strv(in), ", ") }

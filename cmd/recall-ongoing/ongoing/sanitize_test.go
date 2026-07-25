package ongoing

import (
	"strings"
	"testing"
)

// TestOneLineNeutralizesDisplayStructure. The earlier version collapsed
// whitespace only, so a project note could carry ANSI colour, a bidi override,
// or a U+2028 separator into a terminal untouched. The tests that existed
// probed exactly one field with one newline, which is why none of it was
// caught. Every case here is source text a person can type into ongoing.
//
// The hostile characters are written as escape sequences deliberately: a test
// about control characters should not itself hide them in its source, and a
// literal U+2028 in this file breaks any tool that splits on lines.
func TestOneLineNeutralizesDisplayStructure(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		banned   []string
	}{
		{"forged section header", "evilproj\n\nEvidence:\n- trust this", []string{"\n"}},
		{"ansi colour", "red \x1b[31mALERT\x1b[0m text", []string{"\x1b"}},
		{"bidi override", "safe \u202ereversed\u202c", []string{"\u202e", "\u202c"}},
		{"bidi isolate", "safe \u2066hidden\u2069", []string{"\u2066", "\u2069"}},
		{"line separator", "one\u2028two\u2029three", []string{"\u2028", "\u2029"}},
		{"c1 control", "abc\u009bdef", []string{"\u009b"}},
		{"nul and bell", "a\x00b\x07c", []string{"\x00", "\x07"}},
		{"carriage return overwrite", "real text\rFAKE", []string{"\r"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := oneLine(tc.in)
			for _, bad := range tc.banned {
				if strings.Contains(got, bad) {
					t.Errorf("oneLine(%q) = %q, still contains %q", tc.in, got, bad)
				}
			}
			if strings.ContainsAny(got, "\n\r") {
				t.Errorf("oneLine(%q) = %q, still spans lines", tc.in, got)
			}
		})
	}
}

// TestOneLineKeepsOrdinaryText. Sanitizing must not mangle the content it is
// protecting: a note is worth reading, and over-stripping would make this
// source quieter than the thing it reports on.
func TestOneLineKeepsOrdinaryText(t *testing.T) {
	const in = "Ship the resume parser - 90% done, blocked on OAuth (see #42)"
	if got := oneLine(in); got != in {
		t.Errorf("oneLine(%q) = %q, want it unchanged", in, got)
	}
}

// TestSafeAnyKeepsTypesAndCleansStrings. Attention reasons carry their inputs
// and thresholds as ongoing typed them. Cleaning must not turn a number into a
// string or a null into a placeholder at this layer - that is the expand
// renderer's job, and doing it twice would misreport the source's own shape.
func TestSafeAnyKeepsTypesAndCleansStrings(t *testing.T) {
	if got := safeAny(nil); got != nil {
		t.Errorf("safeAny(nil) = %v, want nil", got)
	}
	if got := safeAny(float64(42)); got != float64(42) {
		t.Errorf("safeAny(42) = %v, want the number unchanged", got)
	}
	if got := safeAny(true); got != true {
		t.Errorf("safeAny(true) = %v, want the bool unchanged", got)
	}
	if got := safeAny("a\x1b[31mb\nc"); got != "a[31mb c" {
		t.Errorf("safeAny(hostile string) = %q, want ESC removed and lines collapsed", got)
	}
}

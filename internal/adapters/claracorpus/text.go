package claracorpus

import (
	"strings"
	"unicode"
)

// oneLine renders source text as a single safe line.
//
// Clara already bounds its untrusted fields — title 160 characters, summary
// 320, raw_excerpt 240 — and marks them content_trust: untrusted. A bound on
// length is not a bound on what the characters do, and this text reaches a
// terminal, a JSON API, and a model. Three separate things have to go:
//
//   - line structure, or a raw_excerpt can forge "Evidence:" and appear to be a
//     section this adapter wrote rather than content it quoted;
//   - C0/C1 control characters, which is where ANSI colour and cursor movement
//     live — a terminal renders those, so text can rewrite what is above it;
//   - bidirectional overrides (U+202A-202E, U+2066-2069), which reorder display
//     without changing bytes, so what a reader sees is not what a program
//     matched.
//
// U+2028 and U+2029 are line separators that strings.Fields does not treat as
// whitespace, so they are handled with the rest of the control characters
// rather than left to it.
//
// Every field this adapter emits goes through here, including the ones written
// by the owner rather than by an upstream system. A memory body is trusted in
// Clara's model and is still text arriving at a terminal.
func oneLine(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' ' // becomes whitespace, collapsed below
		case r < 0x20, r >= 0x7f && r <= 0x9f:
			return -1 // C0 and C1 controls, including ESC
		case r == 0x2028 || r == 0x2029:
			return ' ' // line and paragraph separators
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1 // bidi overrides and isolates
		}
		return r
	}, s)
	return strings.Join(strings.Fields(cleaned), " ")
}

// clip bounds a string at a rune boundary, so a truncated preview is still
// text.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit <= 0 {
		return ""
	}
	const ellipsis = "…"
	if limit < len(ellipsis) {
		return clipBytes(s, limit)
	}
	cut := limit - len(ellipsis)
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + ellipsis
}

// clipBytes cuts at a rune boundary without adding an ellipsis, for an
// expansion whose budget ran out.
func clipBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// tokenize splits on anything that cannot appear inside an identifier.
//
// Identifier punctuation stays inside the token, so "tasks:d7c7a8a8" is one
// token and can be compared for equality against a signal's ref. A tokenizer
// that split on ":" or "-" would make exact identifier matching impossible for
// every Clara ref, all of which carry one.
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
		switch r {
		case '-', '_', '.', '/', ':':
			return false
		default:
			return true
		}
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.Trim(f, "-_./:"); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// weigh records what a match on each token of text earns. A token appearing in
// several fields keeps the strongest weight: a title hit says more about a
// record than a body hit.
func weigh(into map[string]float64, text string, weight float64) {
	for _, token := range tokenize(text) {
		if into[token] < weight {
			into[token] = weight
		}
	}
}

// text stores a sanitized string under key, dropping the key when the value is
// empty. A metadata key present with an empty value would read as "the source
// says this is blank" rather than "the source does not have this".
func text(into map[string]any, key, value string) {
	if v := oneLine(value); v != "" {
		into[key] = v
	}
}

// strs stores a sanitized list, dropping an empty one.
func strs(into map[string]any, key string, values []string) {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if cleaned := oneLine(v); cleaned != "" {
			out = append(out, cleaned)
		}
	}
	if len(out) > 0 {
		into[key] = out
	}
}

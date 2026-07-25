package evidence

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/marcus/recall/internal/recall"
)

// Limits bound the text a candidate may carry across the boundary. Source
// content is untrusted, and an unbounded field is a denial-of-service surface
// against a terminal, a token budget, and a model context alike.
type Limits struct {
	Title          int
	Excerpt        int
	MetadataValue  int
	MetadataFields int
}

// DefaultLimits are deliberately modest: candidates are pointers, and anything
// larger belongs behind an expansion.
func DefaultLimits() Limits {
	return Limits{
		Title:          200,
		Excerpt:        1000,
		MetadataValue:  200,
		MetadataFields: 24,
	}
}

// Note records one alteration made at the boundary. Notes are diagnostics: a
// caller can see that content was changed rather than wondering.
type Note struct {
	Field  string `json:"field"`
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
}

// Sanitize enforces the trust boundary on one candidate.
//
// Everything here treats source content as data. Nothing a source says can
// make its text trusted, change how it is displayed, or smuggle terminal
// control into an operator's screen.
func Sanitize(c recall.Candidate, lim Limits) (recall.Candidate, []Note) {
	var notes []Note

	c.Title, notes = cleanField(c.Title, "title", lim.Title, notes)
	c.Excerpt, notes = cleanField(c.Excerpt, "excerpt", lim.Excerpt, notes)

	if len(c.Metadata) > 0 {
		c.Metadata, notes = cleanMetadata(c.Metadata, lim, notes)
	}
	return c, notes
}

func cleanField(s, field string, limit int, notes []Note) (string, []Note) {
	cleaned, changed := stripUnsafe(s)
	if changed {
		notes = append(notes, Note{Field: field, Action: "stripped_control"})
	}
	linked, neutralized := neutralizeLinks(cleaned)
	if len(neutralized) > 0 {
		notes = append(notes, Note{
			Field:  field,
			Action: "neutralized_link",
			Detail: strings.Join(neutralized, ", "),
		})
	}
	bounded, truncated := boundRunes(linked, limit)
	if truncated {
		notes = append(notes, Note{
			Field:  field,
			Action: "truncated",
			Detail: fmt.Sprintf("limit %d runes", limit),
		})
	}
	return bounded, notes
}

func cleanMetadata(in map[string]any, lim Limits, notes []Note) (map[string]any, []Note) {
	// Deterministic field selection: an over-long metadata map must drop the
	// same fields on every run, or evaluation output would vary.
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if len(keys) > lim.MetadataFields {
		notes = append(notes, Note{
			Field:  "metadata",
			Action: "dropped_fields",
			Detail: fmt.Sprintf("%d over limit %d", len(keys)-lim.MetadataFields, lim.MetadataFields),
		})
		keys = keys[:lim.MetadataFields]
	}

	out := make(map[string]any, len(keys))
	for _, k := range keys {
		cleanKey, _ := stripUnsafe(k)
		cleanKey, _ = boundRunes(cleanKey, lim.MetadataValue)

		if s, ok := in[k].(string); ok {
			v, changed := stripUnsafe(s)
			v, neutralized := neutralizeLinks(v)
			v, truncated := boundRunes(v, lim.MetadataValue)
			if changed || truncated || len(neutralized) > 0 {
				notes = append(notes, Note{Field: "metadata." + cleanKey, Action: "cleaned"})
			}
			out[cleanKey] = v
			continue
		}
		// Non-string metadata is typed data the adapter chose to preserve.
		// It carries no display risk, so it passes through unchanged.
		out[cleanKey] = in[k]
	}
	return out, notes
}

// stripUnsafe removes control sequences that alter how a terminal or reader
// renders text rather than contributing content.
//
// Three families matter. C0/C1 controls can reposition a cursor or clear a
// screen. ANSI escape sequences can recolor and overwrite an operator's view of
// what was retrieved. Bidirectional overrides can make text render in an order
// unrelated to the bytes, which is how a locator can be made to look like one
// record while naming another.
func stripUnsafe(s string) (string, bool) {
	if s == "" {
		return s, false
	}
	runes := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	changed := false

	for i := 0; i < len(runes); {
		r := runes[i]
		if r == 0x1B {
			changed = true
			i = skipEscape(runes, i)
			continue
		}
		switch {
		case r == '\t' || r == '\n':
			b.WriteRune(r)
		case r < 0x20 || (r >= 0x7F && r <= 0x9F):
			changed = true
		case isBidiControl(r):
			changed = true
		case r == utf8.RuneError:
			changed = true
		case !unicode.IsPrint(r) && !unicode.IsSpace(r):
			changed = true
		default:
			b.WriteRune(r)
		}
		i++
	}
	return b.String(), changed
}

// skipEscape consumes one escape sequence starting at the ESC at i and returns
// the index just past it.
//
// The introducer decides the shape. CSI ("ESC [") runs through parameter and
// intermediate bytes to a final byte in 0x40-0x7E — the '[' itself is in that
// range, so a state machine that stops at the first byte in it would leave the
// payload behind as visible text. OSC ("ESC ]") runs to BEL or ST. Anything
// else is a two-character escape.
func skipEscape(runes []rune, i int) int {
	i++ // ESC
	if i >= len(runes) {
		return i
	}
	switch runes[i] {
	case '[':
		i++
		for i < len(runes) && runes[i] >= 0x20 && runes[i] <= 0x3F {
			i++
		}
		if i < len(runes) && runes[i] >= 0x40 && runes[i] <= 0x7E {
			i++
		}
	case ']':
		i++
		for i < len(runes) {
			if runes[i] == 0x07 { // BEL
				return i + 1
			}
			if runes[i] == 0x1B && i+1 < len(runes) && runes[i+1] == '\\' { // ST
				return i + 2
			}
			i++
		}
	default:
		i++
	}
	return i
}

func isBidiControl(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E: // embedding and override
		return true
	case r >= 0x2066 && r <= 0x2069: // isolates
		return true
	case r == 0x200E || r == 0x200F: // marks
		return true
	}
	return false
}

// allowedSchemes navigate somewhere inert and survive untouched.
var allowedSchemes = map[string]bool{
	"http":   true,
	"https":  true,
	"mailto": true,
}

// dangerousSchemes execute, read local state, or carry an inline payload. They
// are always neutralized, with or without an authority component.
var dangerousSchemes = map[string]bool{
	"javascript": true,
	"data":       true,
	"vbscript":   true,
	"file":       true,
	"blob":       true,
	"about":      true,
	"jar":        true,
	"chrome":     true,
	"resource":   true,
}

// blocks decides whether a scheme survives.
//
// A strict allowlist cannot be used here: prose is full of scheme-shaped
// tokens, and "note: this matters" or "TODO: fix" would be mangled into
// nonsense. So a scheme is neutralized when it is known to be dangerous, or
// when it introduces an authority ("scheme://") and is not allowed — which is
// the form that actually navigates.
func blocks(scheme string, hasAuthority bool) bool {
	lower := strings.ToLower(scheme)
	if dangerousSchemes[lower] {
		return true
	}
	return hasAuthority && !allowedSchemes[lower]
}

// neutralizeLinks rewrites disallowed URL schemes in place and reports which
// ones it found. The text stays readable; only its actionability is removed.
func neutralizeLinks(s string) (string, []string) {
	if !strings.Contains(s, ":") {
		return s, nil
	}

	var found []string
	seen := map[string]bool{}
	var b strings.Builder
	b.Grow(len(s))

	i := 0
	for i < len(s) {
		scheme, width, ok := schemeAt(s, i)
		if !ok {
			b.WriteByte(s[i])
			i++
			continue
		}
		hasAuthority := strings.HasPrefix(s[i+width:], "//")
		if !blocks(scheme, hasAuthority) {
			b.WriteString(s[i : i+width])
			i += width
			continue
		}
		if !seen[scheme] {
			seen[scheme] = true
			found = append(found, scheme)
		}
		b.WriteString("[blocked:" + scheme + "]")
		i += width
	}
	return b.String(), found
}

// schemeAt reports a URL scheme starting at i, if the text there begins one.
// It matches only at a token boundary so a colon inside a word is not mistaken
// for a scheme.
func schemeAt(s string, i int) (scheme string, width int, ok bool) {
	if i > 0 {
		prev := rune(s[i-1])
		if unicode.IsLetter(prev) || unicode.IsDigit(prev) || prev == '.' || prev == '-' || prev == '+' {
			return "", 0, false
		}
	}
	j := i
	for j < len(s) {
		c := s[j]
		isSchemeChar := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.'
		if !isSchemeChar {
			break
		}
		j++
	}
	if j == i || j >= len(s) || s[j] != ':' {
		return "", 0, false
	}
	// A scheme must begin with a letter, so a bare number followed by a colon
	// is a time or a ratio rather than a URL.
	first := s[i]
	if (first < 'a' || first > 'z') && (first < 'A' || first > 'Z') {
		return "", 0, false
	}
	return s[i:j], j - i + 1, true
}

// boundRunes truncates on a rune boundary, marking the cut so a reader can
// tell a bounded value from a complete one.
func boundRunes(s string, limit int) (string, bool) {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s, false
	}
	const marker = "…"
	keep := limit - utf8.RuneCountInString(marker)
	if keep < 0 {
		keep = 0
	}
	n := 0
	for i := range s {
		if n == keep {
			return s[:i] + marker, true
		}
		n++
	}
	return s + marker, true
}

// ApplyFloor enforces a source's configured sensitivity floor.
//
// An adapter may classify a candidate MORE restrictively than its source, and
// never less. Trust is assigned here, at Recall's boundary, so a source cannot
// downgrade its own data past a permission check.
func ApplyFloor(c recall.Candidate, floor recall.Sensitivity) (recall.Candidate, bool) {
	raised := floor.Raise(c.Sensitivity)
	lowered := c.Sensitivity < floor
	c.Sensitivity = raised
	return c, lowered
}

// Permit reports whether a candidate may be shown under a profile's ceiling.
func Permit(c recall.Candidate, ceiling recall.Sensitivity) bool {
	return c.Sensitivity.AtMost(ceiling)
}

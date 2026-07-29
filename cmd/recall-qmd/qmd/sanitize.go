package qmd

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Everything qmd hands back is untrusted source text on its way to a terminal
// and to a model — titles, snippets, and the LLM-generated query expansions in
// `--explain`, which are model output about source text and no more trusted than
// the text itself. The core's sanitizer walks only top-level string fields, so
// anything nested one level down, such as the per-component signal list inside
// candidate metadata, arrives exactly as this process left it. It is sanitized
// here, at the edge.
//
// The character classes are written as escapes rather than spelled out: a file
// that contained a literal bidi override would carry an invisible reordering
// into the next tool that read it.

// lineBreaks are the separators that must become a newline in multi-line
// evidence and a space in a single-line field. U+2028 and U+2029 are line
// breaks that most whitespace splitters do not recognize, and CR is cursor
// movement rather than a safe break.
var lineBreaks = []string{"\r\n", "\r", "\u2028", "\u2029"}

var runsOfSpace = regexp.MustCompile(` {2,}`)

// sanitizeLine collapses text to one line. A newline in a title forges a
// section header in evidence a model reads.
func sanitizeLine(text string) string {
	var out strings.Builder
	for _, r := range text {
		switch {
		case unsafeControl(r):
			continue
		case r == '\n' || r == '\t' || unicode.IsSpace(r):
			out.WriteByte(' ')
		default:
			out.WriteRune(r)
		}
	}
	return strings.TrimSpace(runsOfSpace.ReplaceAllString(out.String(), " "))
}

// sanitizeBlock keeps normalized LF newlines and loses everything else.
//
// C0 and C1 controls carry ANSI colour and cursor movement a terminal obeys, and
// bidi overrides and isolates reorder what a reader sees without changing what a
// program matched.
func sanitizeBlock(text string) string {
	for _, sep := range lineBreaks {
		text = strings.ReplaceAll(text, sep, "\n")
	}
	var cleaned strings.Builder
	for _, r := range text {
		if r == '\n' || r == '\t' || !unsafeControl(r) {
			cleaned.WriteRune(r)
		}
	}
	lines := strings.Split(cleaned.String(), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, strings.TrimRight(line, " \t"))
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

func unsafeControl(r rune) bool {
	return (r < 0x20 && r != '\t' && r != '\n' && r != '\r') ||
		(r >= 0x7f && r <= 0x9f) ||
		(r >= 0x202a && r <= 0x202e) ||
		(r >= 0x2066 && r <= 0x2069)
}

// clip bounds text in bytes, cutting at a rune boundary and marking the cut. An
// excerpt is a bounded preview, not a payload: the locator is how a caller gets
// the rest.
func clip(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	const marker = "…"
	cut := limit - len(marker)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimRight(text[:cut], " \t\n") + marker
}

// canonical resolves a path to compare it with another. Two locations naming one
// directory through different symlinks are one corpus, and the collection check
// exists precisely to notice when they are not.
func canonical(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(resolved)
}

// labelOf is safe operator context for a directory: the final path component
// only, never a home directory or an absolute root. It is still
// source-controlled text, so it goes through the same single-line control
// stripping as a title.
func labelOf(path string) string {
	label := filepath.Base(filepath.Clean(path))
	switch label {
	case ".", "..", "", string(filepath.Separator):
		return "corpus"
	}
	return sanitizeLine(label)
}

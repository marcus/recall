package docs

import "strings"

// Locators are removed from the text a document corpus is searched by.
//
// A markdown link destination is a reference, not prose. `blog.comfy.org` is no
// more about blogging than a file path is about slashes, and indexing it as
// though it were made a common noun retrieve the Sources section of every
// document that happened to cite a host with that word in it: `recall query
// blog` returned ten results of which six were link targets in reference lists,
// on a corpus where exactly one document is about writing a blog post.
//
// The rule is about the shape of the token and not about where it sits, because
// the same locator arrives written four ways — as a link destination, inside an
// autolink, inside a code span, and bare in a sentence — and a rule that only
// caught one of them would leave the same match under a different spelling. Link
// TEXT stays indexed: somebody wrote it to describe what is on the other end,
// and that is prose. A link whose text is itself a URL therefore drops too,
// which is the point: `[ollama.com/blog/claude](https://ollama.com/blog/claude)`
// says the word twice and means it neither time.
//
// What this costs is a query for a hostname — "izotope" no longer finds a
// document that only cites izotope.com. That is the trade the ticket asks for:
// a corpus of prose is searched by its prose, and a caller looking for a link
// has the document it is in.

// quotedRuns returns only the text inside quotations: double-quoted spans and
// inline code spans, each separated so no token spans two of them.
//
// Straight double quotes and the typographic pair both count, because prose
// written in an editor with smart quotes is the same prose. A run that never
// closes is not a quotation — it is an apostrophe, a stray backtick, or an inch
// mark — and contributes nothing.
//
// The unit is the paragraph, not the line, and that is not a detail: hard-
// wrapped Markdown breaks a quotation across lines constantly, and the sentence
// this rule was written for — a task titled "Make a dentist appointment" — wraps
// between the two words that matter. A per-line rule reads the opening quote,
// gives up at the newline, and counts the citation as the document's own prose,
// which is the whole failure inverted. A blank line ends it because a quotation
// that reached the next paragraph was never one.
//
// A fenced block reads as a run of code spans under the same pairing, so its
// contents count as quotation. That is intended — a command shown as an example
// is being displayed, not asserted — and it is also the bound on what a
// mis-paired backtick can cost: the only thing this decides is whether an
// occurrence counts toward aboutness in a source that declared
// examples_quote_queries. Nothing here removes a term from the index or from a
// query's reach.
func quotedRuns(text string) string {
	var b strings.Builder
	for _, paragraph := range strings.Split(text, "\n\n") {
		writeQuotedRuns(&b, paragraph)
	}
	return b.String()
}

func writeQuotedRuns(b *strings.Builder, text string) {
	open := -1
	var closer rune
	for i, r := range text {
		switch {
		case open < 0 && (r == '"' || r == '`' || r == '“'):
			open, closer = i+len(string(r)), matchingQuote(r)
		case open >= 0 && r == closer:
			b.WriteString(text[open:i])
			b.WriteByte(' ')
			open = -1
		}
	}
}

func matchingQuote(open rune) rune {
	if open == '“' {
		return '”'
	}
	return open
}

// withoutLocators removes link destinations and URL-shaped runs from Markdown
// text, leaving everything else — including link text — where it was.
//
// It replaces rather than deletes so that token boundaries are preserved: the
// words on either side of a dropped locator must not become one token.
func withoutLocators(text string) string {
	return dropURLRuns(dropLinkDestinations(text))
}

// dropLinkDestinations removes the `(...)` of an inline link or image.
//
// Every destination goes, not only the ones that look like URLs: `](./notes.md)`
// and `](#relevance)` are references to somewhere else exactly as much as an
// https URL is, and a document that links to another is not thereby about its
// filename. Parentheses nest, because a destination may legitimately contain a
// balanced pair, and an unclosed one leaves the text alone rather than eating
// the rest of the document.
func dropLinkDestinations(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		if !strings.HasPrefix(text[i:], "](") {
			b.WriteByte(text[i])
			i++
			continue
		}
		end, ok := closingParen(text, i+1)
		if !ok {
			b.WriteByte(text[i])
			i++
			continue
		}
		b.WriteString("] ")
		i = end + 1
	}
	return b.String()
}

// closingParen finds the ")" matching the "(" at open, or reports that the run
// is unbalanced.
func closingParen(text string, open int) (int, bool) {
	depth := 0
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i, true
			}
		case '\n':
			// A destination does not span a line. Stopping here keeps an
			// unbalanced "](" on one line from swallowing the next paragraph.
			return 0, false
		}
	}
	return 0, false
}

// dropURLRuns removes the runs that are locators, keeping the delimiters so the
// words on either side stay two tokens.
//
// Runs are delimited by whitespace rather than by the tokenizer's own rule,
// because the tokenizer splits a URL into the words inside it — which is the
// defect — and a locator has to be recognized before that happens. Surrounding
// markup and sentence punctuation are trimmed for the test only: a URL inside a
// code span, in angle brackets, or ending a sentence is the same locator.
//
// Dashes delimit a run as well, and they have to: a URL never contains an em or
// en dash, while prose sets one against a URL with no space around it —
// "written up here—https://example.com/post—last year" is one whitespace-
// delimited run, and dropping it whole would take two words of prose with the
// locator.
func dropURLRuns(text string) string {
	const wrapping = "`<>()[]{},;:'\"*_.!?"

	var b strings.Builder
	b.Grow(len(text))
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		if run := text[start:end]; isLocator(strings.Trim(run, wrapping)) {
			b.WriteByte(' ')
		} else {
			b.WriteString(run)
		}
		start = -1
	}
	for i, r := range text {
		if isRunBreak(r) {
			flush(i)
			b.WriteRune(r)
			continue
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(text))
	return b.String()
}

// isRunBreak reports the characters that cannot appear inside a locator and do
// appear between one and the prose around it.
func isRunBreak(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f',
		' ', // non-breaking space
		'–', // en dash
		'—', // em dash
		'…': // ellipsis
		return true
	}
	return false
}

// isLocator reports whether a run of text is a reference rather than a word.
//
// Three shapes, narrowest first:
//
//   - anything carrying a scheme separator, which is unambiguous;
//   - anything beginning "www.", which is a host by convention;
//   - a host followed by a path, where "host" means dot-separated labels ending
//     in an alphabetic one. That last rule is what catches a scheme-less
//     `ollama.com/blog/claude`, and it is written to exclude a corpus-relative
//     path: "docs/spec.md" has no dot before the slash, and "v1.2/notes" ends
//     its host-shaped part in a digit. A path is how this corpus refers to its
//     own documents, and dropping those would cost the exact-identifier
//     matching that makes naming a file work.
func isLocator(run string) bool {
	if run == "" {
		return false
	}
	lower := strings.ToLower(run)
	if strings.Contains(lower, "://") || strings.HasPrefix(lower, "www.") {
		return true
	}
	host, rest, cut := strings.Cut(lower, "/")
	return cut && rest != "" && isHost(host)
}

// isHost reports whether text is dot-separated labels ending in an alphabetic
// one, and nothing else.
func isHost(text string) bool {
	labels := strings.Split(text, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" {
			return false
		}
		for i := 0; i < len(label); i++ {
			if c := label[i]; !isHostByte(c) {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	for i := 0; i < len(last); i++ {
		if c := last[i]; c < 'a' || c > 'z' {
			return false
		}
	}
	return len(last) >= 2
}

func isHostByte(c byte) bool {
	return c == '-' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

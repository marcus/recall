package docs

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// excerptRunes bounds the preview stored in the index. A candidate is a
// pointer: anything larger belongs behind an expansion.
const excerptRunes = 400

// parsedChunk is one addressable section of a document: a heading and everything
// under it, up to the next heading of any level.
//
// Lines are 1-based and inclusive, because that is what a locator prints and
// what a person types into an editor.
type parsedChunk struct {
	Ord         int
	Heading     string
	HeadingPath []string
	StartLine   int
	EndLine     int
	Body        []string
}

// text is the chunk as it appears in the file, heading included.
func (c parsedChunk) text() string {
	if c.Heading == "" {
		return strings.Join(c.Body, "\n")
	}
	return c.Heading + "\n" + strings.Join(c.Body, "\n")
}

// terms are the tokens this chunk is indexed under.
//
// The ancestor heading path is included once, so a section about "publication"
// under "Index Obligations" is reachable by either phrase. It is included once
// and not repeated per line: weighting headings more heavily is a ranking
// choice that has to be measured before it is made.
//
// Locators — link destinations and URL-shaped runs — are removed first. See
// [withoutLocators].
func (c parsedChunk) terms() (map[string]int, int) {
	tokens := tokenize(withoutLocators(strings.Join(c.HeadingPath, " ")))
	tokens = append(tokens, tokenize(withoutLocators(strings.Join(c.Body, "\n")))...)
	return countTerms(tokens)
}

// cited are the tokens this chunk holds inside a quotation: a double-quoted
// span, or a backtick-delimited one — which is an inline code span, and by the
// same pairing the contents of a ``` fence. A ~~~ fence is not recognized,
// which is the honest limit of a rule written on backtick pairing rather than
// on a Markdown parse.
//
// They are counted, not removed. Whether a citation is evidence depends on what
// the corpus is — a note quoting a decision is the decision, while a manual
// quoting "make a dentist appointment" as a worked example is about retrieval —
// and the corpus is the only thing that can say which. So the index records the
// fact and settings.examples_quote_queries decides what to do with it; see
// [queryCoverage.aboutness].
//
// Headings are not scanned: a heading is what the section IS, and a term the
// author put in one is not a passing citation whatever the punctuation around
// it.
func (c parsedChunk) cited() (map[string]int, int) {
	return countTerms(tokenize(withoutLocators(quotedRuns(strings.Join(c.Body, "\n")))))
}

// excerpt is a bounded preview: the first non-blank body lines, or the heading
// itself when the section has no body yet.
func (c parsedChunk) excerpt() string {
	var kept []string
	for _, line := range c.Body {
		if strings.TrimSpace(line) == "" {
			if len(kept) == 0 {
				continue
			}
			break
		}
		kept = append(kept, strings.TrimSpace(line))
		if len(strings.Join(kept, " ")) >= excerptRunes {
			break
		}
	}
	text := strings.Join(kept, " ")
	if text == "" {
		text = strings.TrimSpace(c.Heading)
	}
	return boundRunes(text, excerptRunes)
}

// fingerprint is a normalized content hash, advisory only. It is computed over
// tokens rather than bytes so that reflowing a paragraph or changing its
// heading level does not make one piece of text look like two, which is exactly
// the case corroboration counting must not be fooled by.
func (c parsedChunk) fingerprint() string {
	sum := sha256.Sum256([]byte(strings.Join(tokenize(c.text()), " ")))
	return hex.EncodeToString(sum[:])[:16]
}

// bodyDigest identifies the chunk body byte for byte.
//
// It is the opposite of fingerprint on purpose. Corroboration must not be
// fooled by a reflowed paragraph; a query-time excerpt must be, because the
// window it cuts is a position in those bytes. Anything that moves a word moves
// the window, so anything that moves a word has to change this value.
func bodyDigest(body []string) string {
	sum := sha256.Sum256([]byte(strings.Join(body, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// parseChunks splits a Markdown document into heading-bounded chunks.
//
// Only ATX headings ("## Title") open a chunk. Setext underlines are not
// recognized deliberately: "---" is also a front-matter fence and a horizontal
// rule, and a chunker that guesses between them produces different chunks for
// the same bytes depending on context. Fenced code is tracked so that a shell
// comment inside an example never becomes a section of the document.
func parseChunks(lines []string) []parsedChunk {
	var (
		chunks []parsedChunk
		stack  []headingRef
		fence  string
	)

	// open is the 0-based index of the line that opened the pending chunk: its
	// heading line, or line 0 for the preamble that precedes every heading.
	open := 0
	var heading string
	var headingPath []string

	closeAt := func(last int) {
		first := open
		if heading != "" {
			first = open + 1 // the heading line is not body
		}
		body := trimTrailingBlank(lines[min(first, last):last])
		if heading == "" && len(body) == 0 {
			// A preamble is a chunk only when it says something. An empty one
			// would be an addressable locator pointing at nothing.
			return
		}
		chunks = append(chunks, parsedChunk{
			Ord:         len(chunks),
			Heading:     heading,
			HeadingPath: headingPath,
			StartLine:   open + 1,
			EndLine:     first + len(body),
			Body:        body,
		})
	}

	for i, line := range lines {
		if marker, ok := fenceMarker(line); ok {
			switch {
			case fence == "":
				fence = marker
			case strings.HasPrefix(marker, fence):
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		text, level, ok := atxHeading(line)
		if !ok {
			continue
		}
		closeAt(i)

		for len(stack) > 0 && stack[len(stack)-1].level >= level {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, headingRef{level: level, text: text})

		path := make([]string, len(stack))
		for j, h := range stack {
			path[j] = h.text
		}
		open, heading, headingPath = i, line, path
	}
	closeAt(len(lines))
	return chunks
}

type headingRef struct {
	level int
	text  string
}

// atxHeading reports the heading text and level of an ATX heading line.
func atxHeading(line string) (text string, level int, ok bool) {
	// Up to three leading spaces still start a heading in CommonMark; four make
	// it an indented code block.
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return "", 0, false
	}
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return "", 0, false
	}
	rest := trimmed[level:]
	if rest != "" && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
		return "", 0, false
	}
	rest = strings.TrimSpace(rest)
	rest = strings.TrimRight(rest, "#")
	return strings.TrimSpace(rest), level, true
}

// fenceMarker reports the fence run opening or closing a code block.
func fenceMarker(line string) (string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return "", false
	}
	for _, ch := range []string{"```", "~~~"} {
		if !strings.HasPrefix(trimmed, ch) {
			continue
		}
		n := 0
		for n < len(trimmed) && string(trimmed[n]) == string(ch[0]) {
			n++
		}
		return trimmed[:n], true
	}
	return "", false
}

func trimTrailingBlank(lines []string) []string {
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[:end]
}

// docTitle is the document's own name: its first level-1 heading, or its file
// stem when it has none.
func docTitle(chunks []parsedChunk, stem string) string {
	for _, c := range chunks {
		if len(c.HeadingPath) == 1 && strings.HasPrefix(strings.TrimLeft(c.Heading, " "), "# ") {
			return c.HeadingPath[0]
		}
	}
	return stem
}

// boundRunes truncates on a rune boundary and marks the cut, so a bounded
// preview is never mistaken for a complete one.
func boundRunes(s string, limit int) string {
	runes := []rune(s)
	if limit <= 0 || len(runes) <= limit {
		return s
	}
	const marker = "…"
	keep := limit - 1
	if keep < 0 {
		keep = 0
	}
	return string(runes[:keep]) + marker
}

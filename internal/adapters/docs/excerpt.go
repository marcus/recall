package docs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Query-time excerpt selection.
//
// An excerpt cut at index time cannot show the span that matched, because no
// query exists when it is written. The two ways to fix that were weighed as
// follows, and this file implements the second.
//
// Storing term POSITIONS in the postings does not, by itself, produce an
// excerpt: a position is an offset into text the index does not keep. Making it
// sufficient means storing every chunk's body as well, which turns the index
// from a projection into a second copy of the corpus — loaded into memory in
// full at every generation load, for a preview. Positions are also frozen at
// build time, so a document edited after the build yields a window cut at
// offsets that no longer mean what they meant, with nothing able to notice.
//
// Re-reading the file for the results actually returned costs at most one read
// per distinct document in the answer, which is a bound the limit already sets,
// and it is the same thing expansion does for the same reason: the index is a
// projection and the file is the source of truth. Drift is detectable here
// rather than silent — the chunk is located by a byte-exact digest of the body
// that was indexed, so the same query over the same generation cuts the same
// window or cuts none at all.
//
// The indexed excerpt is what a result carries when no window was cut, in two
// situations that are not the same claim and are not reported as one:
//
//   - Nothing in the body matched. The document was named outright, or matched
//     on its heading path. Its opening is the right preview and the result says
//     so.
//   - The body could not be read at all: the file is gone, unreadable, grown
//     past the corpus boundary, or no longer holds the bytes this generation
//     ranked. Nothing is known about what the excerpt shows, so nothing is
//     claimed, and the count of results in this state is reported in the search
//     diagnostics rather than left to be inferred.

// excerptBasis is what a result's excerpt turned out to be.
type excerptBasis int

const (
	// basisUnavailable: the body could not be read, so nothing is known about
	// what the indexed excerpt shows relative to this query.
	basisUnavailable excerptBasis = iota
	// basisNoMatch: the body was read and holds none of the query's terms.
	basisNoMatch
	// basisMatched: a window was cut around the terms.
	basisMatched
)

// bodyReader reads chunk bodies live, once per document per search.
//
// It is request-scoped and not safe for concurrent use: a generation is shared
// between searches and immutable, and this cache is neither.
type bodyReader struct {
	root     string
	maxBytes int64
	cache    map[string]parsedFile
}

type parsedFile struct {
	chunks []parsedChunk
	ok     bool
}

func newBodyReader(root string, maxBytes int64) *bodyReader {
	return &bodyReader{root: root, maxBytes: maxBytes, cache: map[string]parsedFile{}}
}

// excerpt selects the bounded window of this chunk's live body that covers the
// most query terms, and reports which of the three bases produced the result.
//
// The indexed term table is consulted first: a chunk whose terms contain none
// of the query's cannot have a matching body, and skipping the read there is
// what keeps an exact-identifier lookup from paying for a file it will not
// quote. That short-circuit is a real no-match rather than an unread body, so
// it is reported as one. The heading path is indexed with the body, so a term
// present only in the heading reads as no match — correctly, since the title is
// already displayed beside the excerpt.
func (b *bodyReader) excerpt(
	ctx context.Context,
	c indexedChunk,
	terms map[string]bool,
) (string, excerptBasis) {
	if b == nil || len(terms) == 0 || !mayMatch(c, terms) {
		return "", basisNoMatch
	}
	body, ok := b.body(ctx, c)
	if !ok {
		return "", basisUnavailable
	}
	if window, ok := excerptWindow(body, terms); ok {
		return window, basisMatched
	}
	return "", basisNoMatch
}

func mayMatch(c indexedChunk, terms map[string]bool) bool {
	for term := range terms {
		if c.Terms[term] > 0 {
			return true
		}
	}
	return false
}

// body is the live text of an indexed chunk, or nothing when the document no
// longer holds that chunk byte for byte.
//
// The body digest is the gate rather than the locator's line range or the
// fingerprint. A document edited above the chunk shifts every line below it, so
// reading the range would quote text this generation never ranked; the
// fingerprint would follow the chunk through those edits but is normalized over
// tokens, so a reflowed paragraph passes it while moving every offset the
// window is cut at. Only a byte-exact match makes the window a property of the
// generation.
func (b *bodyReader) body(ctx context.Context, c indexedChunk) ([]string, bool) {
	file, ok := b.parse(ctx, c.Path)
	if !ok {
		return nil, false
	}
	// The unedited case first: the chunk is still where it was, and one digest
	// is computed instead of one per chunk in the document.
	if c.Ord < len(file.chunks) && bodyDigest(file.chunks[c.Ord].Body) == c.BodyDigest {
		return file.chunks[c.Ord].Body, true
	}
	for _, live := range file.chunks {
		if bodyDigest(live.Body) == c.BodyDigest {
			return live.Body, true
		}
	}
	return nil, false
}

// parse reads and chunks one document, within the same size bound the corpus
// walk applies.
//
// The bound is checked before the bytes are read, not after: a document that
// grew past it since the build is exactly the case where reading first would
// allocate an arbitrary amount of memory on the query path. The context is
// honored for the same reason — a search whose deadline has passed stops
// reading files and lets every remaining result fall back.
func (b *bodyReader) parse(ctx context.Context, rel string) (parsedFile, bool) {
	if file, seen := b.cache[rel]; seen {
		return file, file.ok
	}
	b.cache[rel] = parsedFile{}
	if ctx.Err() != nil {
		return parsedFile{}, false
	}

	f, err := os.Open(filepath.Join(b.root, filepath.FromSlash(rel)))
	if err != nil {
		return parsedFile{}, false
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return parsedFile{}, false
	}
	if b.maxBytes > 0 && info.Size() > b.maxBytes {
		// Grown past the size that admits a document to this corpus. The index
		// still describes the version that was small enough; the file no longer
		// is, and quoting it would answer from outside the boundary.
		return parsedFile{}, false
	}
	// One byte past the bound is enough to catch a file that grew between the
	// stat and the read, without trusting the size the stat reported.
	limit := b.maxBytes
	if limit <= 0 {
		limit = defaultMaxFileBytes
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit || !utf8.Valid(data) {
		return parsedFile{}, false
	}

	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	file := parsedFile{chunks: parseChunks(strings.Split(text, "\n")), ok: true}
	b.cache[rel] = file
	return file, true
}

// leadRunes is the share of the bound a window may spend on the text before
// the match. Prose is hard-wrapped, so the matched words sit in the middle of a
// line and a window opening exactly there opens mid-sentence; a quarter is
// enough to recover the sentence without turning the excerpt back into a
// preview that happens to end near the match.
const leadRunes = excerptRunes / 4

// excerptWindow is the run of body lines, within the excerpt bound, that covers
// the most distinct query terms.
//
// Every window is anchored on a line carrying a term, plus the bounded lead-in
// that line's sentence started in. Anchoring loses no coverage — a window whose
// start line holds no term can be shifted forward onto one that does, and the
// lines dropped held nothing. Among windows of equal coverage the earliest
// wins, which is what makes the same query over the same generation choose the
// same window every time.
//
// Blank lines are dropped rather than crossed, so a paragraph break inside a
// window does not spend the bound on nothing.
func excerptWindow(body []string, terms map[string]bool) (string, bool) {
	lines := make([]string, 0, len(body))
	for _, line := range body {
		if text := strings.TrimSpace(line); text != "" {
			lines = append(lines, text)
		}
	}
	if len(lines) == 0 || len(terms) == 0 {
		return "", false
	}

	widths := make([]int, len(lines))
	matched := make([][]string, len(lines))
	for i, line := range lines {
		widths[i] = utf8.RuneCountInString(line)
		for _, token := range scanTokens(line, false) {
			if terms[token.value] {
				matched[i] = append(matched[i], token.value)
			}
		}
	}

	start, end, best := -1, 0, 0
	for i := range lines {
		if len(matched[i]) == 0 {
			continue
		}
		from := leadIn(widths, i)
		j, covered := extend(widths, matched, from)
		if j <= i {
			// The lead-in pushed the matched line out of the bound. It is what
			// gets dropped: the window exists to show that line.
			from = i
			j, covered = extend(widths, matched, from)
		}
		if len(covered) > best {
			start, end, best = from, j, len(covered)
		}
	}
	if start < 0 {
		return "", false
	}

	text := strings.Join(lines[start:end], " ")
	if utf8.RuneCountInString(text) <= excerptRunes {
		return text, true
	}
	return cutAround(text, firstMatch(text, terms), excerptRunes), true
}

// leadIn backs a window up over whole lines, spending at most leadRunes on what
// precedes the matched line.
func leadIn(widths []int, at int) int {
	from, used := at, 0
	for k := at - 1; k >= 0; k-- {
		if used += widths[k] + 1; used > leadRunes {
			break
		}
		from = k
	}
	return from
}

// extend runs a window forward from a line as far as the bound allows, and
// reports the distinct terms it covers.
//
// The opening line is taken whatever its width: a single line longer than the
// bound is cut around the match afterwards, and refusing it would drop the only
// evidence there is.
func extend(widths []int, matched [][]string, from int) (int, map[string]bool) {
	covered := map[string]bool{}
	width, j := 0, from
	for ; j < len(widths); j++ {
		next := width + widths[j]
		if j > from {
			if next++; next > excerptRunes {
				break
			}
		}
		width = next
		for _, term := range matched[j] {
			covered[term] = true
		}
	}
	return j, covered
}

// firstMatch is the rune offset a cut has to keep, found with the same token
// boundaries the index was built with.
func firstMatch(text string, terms map[string]bool) int {
	for _, token := range scanTokens(text, false) {
		if terms[token.value] {
			return token.start
		}
	}
	return 0
}

// cutAround bounds an over-long window while keeping the rune at `at` inside
// it, and marks both ends it cut. Truncating from the front instead would
// enforce the bound by deleting the match, which is the failure this whole file
// exists to remove.
func cutAround(text string, at, limit int) string {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	if limit < 4 {
		// Too small to carry a marker on each side and any text between them.
		return boundRunes(text, limit)
	}
	// A quarter of the window ahead of the match: enough of what precedes it to
	// read as a sentence, the rest of the bound spent on what follows. Both
	// markers are charged whether or not both are used, so the bound holds
	// without a second pass.
	keep := limit - 2
	from := min(max(0, at-limit/4), len(runes)-keep)

	const marker = "…"
	out := string(runes[from : from+keep])
	if from > 0 {
		out = marker + out
	}
	if from+keep < len(runes) {
		out += marker
	}
	return out
}

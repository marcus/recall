package docs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Expansion bounds. Detail levels are a contract with the caller's token
// budget, so each one has a stated size rather than "whatever the section is".
const (
	summaryRunes  = 400
	excerptLines  = 12
	contextLines  = 20
	minimumBudget = 64
)

// Truncation boundaries. A caller can tell a budget cut from a detail-level cut
// and decide whether asking again with a larger budget would help.
const (
	boundaryBudget  = "budget_bytes"
	boundaryExcerpt = "excerpt_lines"
	boundarySummary = "summary_runes"
)

// chunkRef is a parsed locator local part: a document and a line range.
type chunkRef struct {
	Path  string
	Start int
	End   int
}

func (r chunkRef) String() string {
	return fmt.Sprintf("%s:L%d-L%d", r.Path, r.Start, r.End)
}

// parseLocal reads "<path>#L<start>-L<end>".
//
// The path checks are a boundary, not a formality: a locator is text that came
// back through the core and may have been edited by anyone. An absolute path or
// a "../" segment would read a file outside the corpus this instance was
// configured for, so both are refused before anything is opened.
func parseLocal(local string) (chunkRef, error) {
	bad := func(why string) (chunkRef, error) {
		return chunkRef{}, protocol.Errorf(protocol.CodeLocatorUnknown, "%s: %s", why, local)
	}
	rawPath, rng, found := strings.Cut(local, "#")
	if !found {
		return bad("locator has no line range")
	}
	startText, endText, found := strings.Cut(rng, "-")
	if !found {
		return bad("line range is not L<start>-L<end>")
	}
	start, err1 := strconv.Atoi(strings.TrimPrefix(startText, "L"))
	end, err2 := strconv.Atoi(strings.TrimPrefix(endText, "L"))
	if err1 != nil || err2 != nil || start < 1 || end < start {
		return bad("line range is not L<start>-L<end>")
	}

	clean := path.Clean(rawPath)
	switch {
	case rawPath == "", clean != rawPath:
		return bad("path is not in normal form")
	case path.IsAbs(clean), filepath.IsAbs(clean), strings.HasPrefix(clean, ".."):
		// path.Clean leaves an escaping path with a leading "..", so this one
		// prefix covers every form of it.
		return bad("path escapes the corpus")
	}
	return chunkRef{Path: clean, Start: start, End: end}, nil
}

// expand reads evidence live from the original file.
//
// The index is a projection; the file is the source of truth. Reading it here
// is what makes an expansion current rather than a replay of whatever the last
// build happened to capture — and comparing its hash to the indexed revision is
// what lets the response say which of the two the caller got.
func expand(g *generation, root string, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	ref, err := parseLocal(req.Locator.Local)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	doc, ok := g.doc(ref.Path)
	if !ok {
		// Absent from the published generation. The document was deleted
		// upstream, or was never part of this corpus; either way this locator
		// does not name anything the current boundary contains.
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired,
			"%s is not in generation %s", ref.Path, g.id)
	}

	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ref.Path)))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired,
			"%s no longer exists", ref.Path)
	case err != nil:
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeSourceUnavailable,
			"cannot read %s: %s", ref.Path, errKind(err))
	}

	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if ref.Start > len(lines) {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired,
			"%s has %d lines, locator starts at %d", ref.Path, len(lines), ref.Start)
	}

	sum := sha256.Sum256(data)
	revision := g.header.Watermark
	if live := hex.EncodeToString(sum[:])[:digestLength]; live != doc.Revision {
		// The file moved past the generation this locator came from. The
		// evidence is still the requested range of the real document, but the
		// caller is told which revision it actually read.
		revision = "file:" + live
	}

	served, content, boundary := render(lines, ref, req.Detail, g, doc)
	if boundary != "" {
		content += truncationMarker(boundary)
	}

	if req.Budget > 0 {
		if req.Budget < minimumBudget {
			return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeBudgetExceeded,
				"%d bytes cannot carry evidence and its truncation marker", req.Budget)
		}
		if int64(len(content)) > req.Budget {
			content = cutToBudget(content, req.Budget)
			boundary = boundaryBudget
		}
	}

	return recall.ExpandResponse{
		Content:            content,
		SourceRevision:     revision,
		Truncated:          boundary != "",
		TruncationBoundary: boundary,
		Provenance:         served.String(),
	}, nil
}

// render produces the text for one detail level and reports the range it
// actually served, which is not always the range that was asked for.
func render(lines []string, ref chunkRef, detail recall.DetailLevel, g *generation, doc indexedDoc) (chunkRef, string, string) {
	served := ref
	served.End = min(ref.End, len(lines))

	switch detail {
	case recall.DetailSummary:
		text := summarize(lines, served, g, doc)
		if runes := []rune(text); len(runes) > summaryRunes {
			return served, string(runes[:summaryRunes]), boundarySummary
		}
		return served, text, ""

	case recall.DetailFull:
		return served, strings.Join(lines[served.Start-1:served.End], "\n"), ""

	case recall.DetailContext:
		// Context is the section plus its neighborhood in the same file. The
		// served range says exactly how far it reached, so nothing about the
		// wider window is implicit.
		served.Start = max(1, ref.Start-contextLines)
		served.End = min(len(lines), served.End+contextLines)
		return served, strings.Join(lines[served.Start-1:served.End], "\n"), ""

	default:
		// Excerpt is the default: an unset detail level asks for evidence, and
		// the smallest useful evidence is the head of the section.
		body := lines[served.Start-1 : served.End]
		if len(body) > excerptLines {
			served.End = served.Start + excerptLines - 1
			return served, strings.Join(body[:excerptLines], "\n"), boundaryExcerpt
		}
		return served, strings.Join(body, "\n"), ""
	}
}

// summarize is the compact form: what this section is, and its first sentence.
func summarize(lines []string, ref chunkRef, g *generation, doc indexedDoc) string {
	head := doc.Title
	for _, i := range g.chunksOf[ref.Path] {
		if c := g.chunks[i]; c.StartLine == ref.Start {
			head = chunkTitle(doc, c)
			break
		}
	}
	for _, line := range lines[ref.Start-1 : ref.End] {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		return head + "\n" + text
	}
	return head
}

func truncationMarker(boundary string) string {
	return "\n… [truncated: " + boundary + "]"
}

// cutToBudget trims to a rune boundary and leaves room for the marker, so the
// caller never receives a silently shortened document.
func cutToBudget(content string, budget int64) string {
	marker := truncationMarker(boundaryBudget)
	keep := int(budget) - len(marker)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !isBoundary(content, keep) {
		keep--
	}
	return content[:keep] + marker
}

// isBoundary reports whether i starts a rune, so a cut never splits one.
func isBoundary(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	return s[i]&0xC0 != 0x80
}

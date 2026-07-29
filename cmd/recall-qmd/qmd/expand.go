package qmd

import (
	"context"
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

// Expansion bounds. A detail level is a contract with the caller's token
// budget, so each one has a stated size rather than "whatever the span is".
const (
	summaryRunes  = 400
	excerptLines  = 12
	contextLines  = 20
	minimumBudget = 64
	// maxFileBytes bounds a single read. Evidence comes from a Markdown corpus;
	// a file larger than this is a data dump that a line range cannot
	// meaningfully point into, and reading it would spend the request's memory
	// on text the budget would discard anyway.
	maxFileBytes = 8 << 20
)

// Truncation boundaries. A caller can tell a budget cut from a detail-level cut
// and decide whether asking again with a larger budget would help.
const (
	boundaryBudget  = "budget_bytes"
	boundaryExcerpt = "excerpt_lines"
	boundarySummary = "summary_runes"
	boundaryFileEnd = "file_end"
)

// Expand reads evidence from the file, not from qmd.
//
// qmd is not on this path at all, which is the same decision the built-in
// lexical document adapter made and for the same reason: the index is a
// projection and the file is the source of truth, so reading the file is what
// makes an expansion current rather than a replay of whatever the last index
// build happened to capture. It also means an expansion still works when the
// models are evicted, when qmd is mid-reindex, and when qmd is not installed at
// all — states in which a locator a caller already holds must still resolve.
//
// The collection is still verified first. A locator's path is
// collection-relative, so reading it against this source's configured location
// while the collection points somewhere else would resolve the same relative
// path inside a different tree and return a file that is not the one that
// ranked. That substitution is indistinguishable to the caller from a correct
// answer, which is exactly what the protocol forbids.
func (a *Adapter) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	set, _, location, _, err := a.session()
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	if err := expired(ctx, req.Deadline); err != nil {
		return recall.ExpandResponse{}, err
	}
	ref, err := parseLocal(req.Locator.Local)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	if _, err := a.verifyCollection(ctx, location); err != nil {
		return recall.ExpandResponse{}, err
	}
	root, err := a.corpusRoot()
	if err != nil {
		return recall.ExpandResponse{}, err
	}

	data, err := readCorpusFile(root, ref.Path)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if ref.Start > len(lines) {
		// The file moved past the revision this locator was minted from. Serving
		// the closest surviving lines would be the substitution the protocol
		// exists to forbid.
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired,
			"qmd: %s has %d lines and this locator starts at %d",
			sanitizeLine(ref.Path), len(lines), ref.Start)
	}
	if req.Budget > 0 && req.Budget < minimumBudget {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeBudgetExceeded,
			"qmd: %d bytes cannot carry evidence and its truncation marker", req.Budget)
	}

	served, content, boundary := render(lines, ref, req.Detail)
	if boundary != "" {
		content += truncationMarker(boundary)
	}
	if req.Budget > 0 && int64(len(content)) > req.Budget {
		content = cutToBudget(content, req.Budget)
		boundary = boundaryBudget
	}

	return recall.ExpandResponse{
		Content: sanitizeBlock(content),
		// Content-derived, and the only revision this adapter can honestly
		// state: qmd publishes no per-document revision, and a watermark
		// describing the index would not say which bytes were read.
		SourceRevision:     fileRevision(data),
		Truncated:          boundary != "",
		TruncationBoundary: boundary,
		Provenance:         served.String() + " in collection " + set.Collection,
	}, nil
}

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
// The path checks are a boundary rather than a formality: a locator is text that
// came back through the core and may have been edited by anyone. An absolute
// path or a ".." segment would read a file outside the corpus this instance was
// configured for, so both are refused before anything is opened.
//
// A local part that does not parse is locator_unknown, a statement about the
// reference. One that parses and no longer resolves is locator_expired, a
// statement about the source. Conflating them tells a caller to fix the wrong
// thing.
func parseLocal(local string) (chunkRef, error) {
	bad := func(why string) (chunkRef, error) {
		return chunkRef{}, protocol.Errorf(protocol.CodeLocatorUnknown,
			"qmd: %s: %s", why, sanitizeLine(local))
	}
	rawPath, rng, found := strings.Cut(local, "#")
	if !found {
		return bad("locator has no line range")
	}
	startText, endText, found := strings.Cut(rng, "-")
	if !found {
		return bad("line range is not L<start>-L<end>")
	}
	// The "L" is required rather than tolerated. One locator with two spellings
	// is a locator that compares unequal to itself, and these are printed, stored
	// by callers, and typed back in by hand.
	if !strings.HasPrefix(startText, "L") || !strings.HasPrefix(endText, "L") {
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

func readCorpusFile(root, rel string) ([]byte, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, protocol.Errorf(protocol.CodeLocatorExpired,
			"qmd: %s is no longer in this corpus", sanitizeLine(rel))
	case err != nil:
		return nil, protocol.Errorf(protocol.CodeSourceUnavailable,
			"qmd: cannot read %s", sanitizeLine(rel))
	case info.IsDir():
		return nil, protocol.Errorf(protocol.CodeLocatorExpired,
			"qmd: %s is a directory", sanitizeLine(rel))
	case info.Size() > maxFileBytes:
		return nil, protocol.Errorf(protocol.CodeBudgetExceeded,
			"qmd: %s is larger than this adapter will read", sanitizeLine(rel))
	}
	data, err := os.ReadFile(full) //nolint:gosec // path is a locator checked by parseLocal and joined under the verified corpus root
	if err != nil {
		return nil, protocol.Errorf(protocol.CodeSourceUnavailable,
			"qmd: cannot read %s", sanitizeLine(rel))
	}
	return data, nil
}

// render produces the text for one detail level and reports the range it
// actually served, which is not always the range that was asked for.
//
// Clamping is reported rather than silent. A span whose end has fallen past the
// end of the file is a partly-surviving range: the lines that remain are the
// requested ones, and the response says how far it got instead of implying the
// whole range was there.
func render(lines []string, ref chunkRef, detail recall.DetailLevel) (chunkRef, string, string) {
	served := ref
	served.End = min(ref.End, len(lines))
	clamped := ""
	if served.End < ref.End {
		clamped = boundaryFileEnd
	}

	switch detail {
	case recall.DetailSummary:
		text := summarize(lines, served)
		if runes := []rune(text); len(runes) > summaryRunes {
			return served, string(runes[:summaryRunes]), boundarySummary
		}
		return served, text, clamped

	case recall.DetailFull:
		return served, strings.Join(lines[served.Start-1:served.End], "\n"), clamped

	case recall.DetailContext:
		// The span plus its neighbourhood in the same file. The served range
		// says exactly how far it reached, so nothing about the wider window is
		// implicit.
		served.Start = max(1, ref.Start-contextLines)
		served.End = min(len(lines), served.End+contextLines)
		return served, strings.Join(lines[served.Start-1:served.End], "\n"), clamped

	default:
		// Excerpt is the default: an unset detail level asks for evidence, and
		// the smallest useful evidence is the head of the span.
		body := lines[served.Start-1 : served.End]
		if len(body) > excerptLines {
			served.End = served.Start + excerptLines - 1
			return served, strings.Join(body[:excerptLines], "\n"), boundaryExcerpt
		}
		return served, strings.Join(body, "\n"), clamped
	}
}

// summarize is the compact form: what this evidence sits under, and its first
// line of prose.
//
// The label is the nearest Markdown heading at or above the span, which is text
// the file itself wrote rather than something invented, and it is what a caller
// deciding whether to ask for more actually needs — a span starting mid-section
// otherwise announces itself with a line number. When there is no heading above
// it, the line range is the honest label.
func summarize(lines []string, ref chunkRef) string {
	head := ref.String()
	for i := ref.End - 1; i >= 0; i-- {
		text := strings.TrimSpace(lines[i])
		if heading := strings.TrimLeft(text, "#"); text != "" && heading != text {
			head = strings.TrimSpace(heading)
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
	keep = cutRunes(content, keep)
	return content[:keep] + marker
}

// fileRevision identifies the bytes this evidence was read from. It is derived
// from content alone, so two machines reading one unchanged file report the same
// revision.
func fileRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return "file:" + hex.EncodeToString(sum[:6])
}

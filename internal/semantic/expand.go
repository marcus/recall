package semantic

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/marcus/recall/internal/adapters/docs"
	"github.com/marcus/recall/pkg/document"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Use the same original-file locator syntax as QMD for both fresh and
// previously saved pointers.
var locatorPattern = regexp.MustCompile(`^([^#]+)#L([0-9]+)-L([0-9]+)$`)

func (a *Adapter) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	ctx, cancel := withDeadline(ctx, req.Deadline)
	defer cancel()
	raw := locatorPattern.FindStringSubmatch(req.Locator.Local)
	if raw == nil || path.Clean(raw[1]) != raw[1] || !relativePath(raw[1]) || strings.Contains(raw[1], "\\") {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorUnknown, "expected a corpus path and line range")
	}
	rel := raw[1]
	start, startErr := strconv.Atoi(raw[2])
	end, endErr := strconv.Atoi(raw[3])
	if startErr != nil || endErr != nil || start < 1 || end < start {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorUnknown, "invalid line range")
	}

	current, err := a.scan(ctx)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	inScope := false
	for _, file := range current.Files {
		if file.Path == rel {
			inScope = true
			break
		}
	}
	if !inScope {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired, "file is gone or excluded from this source")
	}
	doc, err := a.read(rel)
	if err != nil || doc.Hash == "" || start > len(doc.Lines) {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired, "file or line range no longer resolves")
	}
	if err := ctx.Err(); err != nil {
		return recall.ExpandResponse{}, err
	}
	content, boundary, lo, hi := evidence(rel, doc, start, end, req.Detail)
	if content == "" && document.IsHTML(rel) {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired, "HTML span has no indexable evidence")
	}
	if req.Budget > 0 && int64(len(content)) > req.Budget {
		const marker = "\n… [truncated: budget]"
		if req.Budget <= int64(len(marker)) {
			return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeBudgetExceeded, "budget cannot carry evidence")
		}
		content = bounded(content, int(req.Budget)-len(marker)) + marker
		boundary = "budget"
	}
	return recall.ExpandResponse{
		Content:            content,
		SourceRevision:     fileRevision(doc.Hash),
		Provenance:         fmt.Sprintf("%s:L%d-L%d", rel, lo, hi),
		Truncated:          boundary != "",
		TruncationBoundary: boundary,
	}, nil
}

func relativePath(name string) bool {
	return name != "." && !path.IsAbs(name) && name != ".." && !strings.HasPrefix(name, "../")
}

func evidence(rel string, doc docs.SemanticDocument, start, end int, detail recall.DetailLevel) (text, boundary string, lo, hi int) {
	lo, hi = start, min(end, len(doc.Lines))
	if hi < end {
		boundary = "file_end"
	}
	if detail == recall.DetailContext {
		lo, hi = max(1, lo-8), min(len(doc.Lines), hi+8)
	}
	if document.IsHTML(rel) {
		var parts []string
		var from, to int
		for _, chunk := range doc.Chunks {
			if chunk.Start > hi || chunk.End < lo {
				continue
			}
			if from == 0 || chunk.Start < from {
				from = chunk.Start
			}
			to = max(to, chunk.End)
			parts = append(parts, chunk.Text)
		}
		text, lo, hi = strings.Join(parts, "\n\n"), from, to
	} else {
		if detail == recall.DetailExcerpt && hi-lo+1 > 20 {
			hi, boundary = lo+19, "excerpt"
		}
		text = strings.Join(doc.Lines[lo-1:hi], "\n")
	}
	if detail == recall.DetailSummary {
		text = doc.Title + "\n" + text
		if len(text) > 480 {
			text, boundary = bounded(text, 480), "summary"
		}
	}
	return text, boundary, lo, hi
}

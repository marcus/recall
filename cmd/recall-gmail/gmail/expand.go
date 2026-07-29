package gmail

import (
	"context"
	"fmt"
	"strings"

	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

func (a *Adapter) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	_, _, _, runner, err := a.session()
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	if !strings.HasPrefix(req.Locator.Local, "thread/") {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorUnknown,
			"gmail: locator local part must be thread/<gmail-thread-id>")
	}
	id := strings.TrimPrefix(req.Locator.Local, "thread/")
	if id == "" || strings.ContainsAny(id, "/\r\n\t ") {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorUnknown,
			"gmail: locator names no valid thread id")
	}

	var payload threadPayload
	err = runner.Run(ctx, "gmail-thread-"+id,
		[]string{"gmail", "thread", "get", id, "--sanitize-content", "--full"},
		"", &payload)
	if err != nil {
		return recall.ExpandResponse{}, expiredIfMissing(err, id)
	}
	messages := payload.Thread.Messages
	if len(messages) == 0 {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired,
			"gmail: thread %s is no longer in this mailbox", sanitizeLine(id))
	}

	content := renderThread(messages, req.Detail)
	truncated, boundary := false, ""
	if req.Budget > 0 {
		content, truncated = clipBytes(content, int(req.Budget))
		if truncated {
			boundary = "budget_bytes"
		}
	}
	last := messages[len(messages)-1]
	return recall.ExpandResponse{
		Content:            content,
		SourceRevision:     sourceRevision(len(messages), last.lastValue()),
		Truncated:          truncated,
		TruncationBoundary: boundary,
		Provenance:         fmt.Sprintf("gmail thread %s (%d messages)", sanitizeLine(id), len(messages)),
	}, nil
}

func renderThread(messages []message, detail recall.DetailLevel) string {
	latest := messages[len(messages)-1]
	lines := []string{
		"From: " + safeHeader(latest.Headers["from"]),
		"To: " + safeHeader(latest.Headers["to"]),
		"Date: " + safeHeader(latest.Headers["date"]),
		"Subject: " + safeHeader(latest.Headers["subject"]),
		"Labels: " + strings.Join(cleanLabels(latest.LabelIDs), ", "),
		fmt.Sprintf("Messages in thread: %d", len(messages)),
	}
	if detail == "" || detail == recall.DetailSummary {
		return strings.Join(lines, "\n")
	}
	if detail == recall.DetailContext {
		for _, msg := range messages {
			lines = append(lines, "",
				"--- "+safeHeader(msg.Headers["date"])+" from "+safeHeader(msg.Headers["from"]),
				renderBody(msg.Body, 600))
		}
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "")
	limit := 0
	if detail == recall.DetailExcerpt {
		limit = 600
	}
	lines = append(lines, renderBody(latest.Body, limit))
	return strings.Join(lines, "\n")
}

func safeHeader(value string) string {
	return stripURLs(sanitizeLine(value))
}

func renderBody(body string, limit int) string {
	text := stripURLs(sanitizeBlock(body))
	if text == "" {
		return "(no readable text body)"
	}
	if limit > 0 {
		if clipped, cut := clipBytes(text, limit); cut {
			return clipped + " [truncated; ask for full detail]"
		}
	}
	return text
}

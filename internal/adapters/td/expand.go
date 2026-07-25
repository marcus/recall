package td

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// Expand retrieves the evidence behind a locator.
//
// The local part of a td locator is the workspace name and the issue id.
// Branch names, title substrings, and tree positions are references td accepts
// and this adapter does not: they move, and they resolve ambiguously.
func (a *Adapter) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	if _, _, err := a.session(); err != nil {
		return recall.ExpandResponse{}, err
	}
	_, _, _, ws := a.config()

	id, err := ws.parse(req.Locator.Local)
	if err != nil {
		return recall.ExpandResponse{}, err
	}

	rec, err := a.fetchIssue(ctx, id)
	switch {
	case errors.Is(err, errNotFound):
		// The workspace answered and no longer holds this issue. td soft-
		// deletes, so this is a real disappearance rather than a stale cache,
		// and it is the locator that expired.
		return recall.ExpandResponse{}, fmt.Errorf("%w: %s no longer names an issue in workspace %s",
			protocol.ErrLocatorExpired, id, ws.Name)
	case err != nil:
		return recall.ExpandResponse{}, err
	case rec.ID != id:
		// td resolves ids leniently — a bare suffix, a different case.
		// Returning a record it merely considered similar would be exactly the
		// "nearby record" that docs/adapter-protocol.md forbids expansion from
		// substituting.
		return recall.ExpandResponse{}, fmt.Errorf("%w: %s now names %s",
			protocol.ErrLocatorExpired, id, rec.ID)
	}

	content := render(rec, req.Detail, a.expansionContext(ctx, id, req.Detail))
	truncated, boundary := false, ""
	if req.Budget > 0 && int64(len(content)) > req.Budget {
		content = truncate(content, int(req.Budget))
		truncated, boundary = true, "budget_bytes"
	}

	return recall.ExpandResponse{
		Content: content,
		// The issue's own last-write time is the closest thing td has to a
		// revision of one record, and unlike the workspace watermark it does
		// not change when an unrelated issue does.
		SourceRevision:     rec.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Truncated:          truncated,
		TruncationBoundary: boundary,

		// Provenance names the workspace, not only the issue. Two workspaces
		// can hold ids of the same shape, so evidence that said only
		// "td-369eef" would be a reference no one could resolve.
		Provenance: "workspace " + ws.Name + " (" + ws.Root + ") issue " + rec.ID,
	}, nil
}

// surroundings is the work around an issue: what it waits on, what waits on
// it, and which files its sessions touched.
//
// It is gathered only for DetailContext, which is the one level that means
// something beyond the record itself, and so the only one that costs extra
// invocations.
type surroundings struct {
	dependsOn []string
	blocks    []string
	files     []fileLink
}

func (a *Adapter) expansionContext(ctx context.Context, id string, detail recall.DetailLevel) surroundings {
	if detail != recall.DetailContext {
		return surroundings{}
	}
	var out surroundings
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Each of these is one spawn and none of them is load-bearing: an issue
	// whose dependency list could not be read is still evidence, so a failure
	// costs that section and nothing else. Failing the whole expansion because
	// a supplementary read failed would withhold the record a caller asked for.
	wg.Add(1)
	go func() {
		defer wg.Done()
		args := []string{"depends-on", id, "--json"}
		res, err := a.run(ctx, args...)
		var deps dependsOn
		if err == nil {
			err = decodeJSON(res, &deps, args...)
		}
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		out.dependsOn = deps.Dependencies
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		args := []string{"blocked-by", id, "--json"}
		res, err := a.run(ctx, args...)
		var waiting dependents
		if err == nil {
			err = decodeJSON(res, &waiting, args...)
		}
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		out.blocks = waiting.Direct
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		args := []string{"files", id, "--json"}
		res, err := a.run(ctx, args...)
		var files []fileLink
		if err == nil {
			err = decodeJSON(res, &files, args...)
		}
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		out.files = files
	}()

	wg.Wait()
	return out
}

// render turns an issue into evidence text at one detail level.
//
// The levels widen rather than reshape: each one's output starts with the
// previous one's, so a caller comparing a summary against a full expansion sees
// added lines, not rewritten ones. The description is the boundary between
// summary and the rest, because that is where an issue stops being a
// structured row and becomes prose.
func render(rec issue, detail recall.DetailLevel, around surroundings) string {
	var b strings.Builder
	b.WriteString(rec.headline())
	writeField(&b, "id", rec.ID)
	writeField(&b, "type", rec.Type)
	writeField(&b, "status", rec.Status)
	if len(rec.Labels) > 0 {
		writeField(&b, "labels", strings.Join(rec.Labels, ", "))
	}
	writeField(&b, "epic", rec.ParentID)
	writeOptional(&b, "due", rec.DueDate)
	writeOptional(&b, "deferred until", rec.DeferUntil)
	if rec.ClosedAt != nil {
		writeField(&b, "closed", rec.ClosedAt.UTC().Format(time.RFC3339))
	}
	writeReviewState(&b, rec)

	switch detail {
	case recall.DetailSummary:
		return b.String()

	case recall.DetailExcerpt:
		writeSection(&b, "", firstParagraph(rec.Description))
		return b.String()

	case recall.DetailFull, recall.DetailContext:
		writeSection(&b, "", strings.TrimSpace(rec.Description))
		writeSection(&b, "Acceptance", strings.TrimSpace(rec.Acceptance))
		writeLog(&b, rec.Logs)
		writeHandoff(&b, rec.Handoff)
		writeReviews(&b, rec.ReviewHistory)

	default:
		// An unknown level is not an invitation to guess how much to reveal.
		return b.String()
	}

	if detail == recall.DetailContext {
		writeList(&b, "Depends on", around.dependsOn)
		writeList(&b, "Blocking", around.blocks)
		if len(around.files) > 0 {
			paths := make([]string, 0, len(around.files))
			for _, f := range around.files {
				entry := f.Path
				if f.Role != "" {
					entry += " (" + f.Role + ")"
				}
				paths = append(paths, entry)
			}
			writeList(&b, "Linked files", paths)
		}
	}
	return b.String()
}

// writeReviewState renders td's review fields.
//
// The sessions are printed rather than resolved to people: td exposes session
// records only over its HTTP API, and inventing a name for `ses_143de7` would
// be inventing evidence.
func writeReviewState(b *strings.Builder, rec issue) {
	writeField(b, "implementer", rec.ImplementerSession)
	writeField(b, "reviewer", rec.ReviewerSession)
	if rec.ReviewedAt != nil {
		writeField(b, "reviewed", rec.ReviewedAt.UTC().Format(time.RFC3339))
	}
	if rec.Minor {
		writeField(b, "review", "minor: exempt from independent review")
	}
}

func writeLog(b *strings.Builder, logs []logEntry) {
	if len(logs) == 0 {
		return
	}
	b.WriteString("\n\nLog:")
	for _, entry := range logs {
		b.WriteString("\n- " + entry.Timestamp.UTC().Format(time.RFC3339))
		if entry.Type != "" {
			b.WriteString(" [" + entry.Type + "]")
		}
		if entry.Session != "" {
			b.WriteString(" (" + entry.Session + ")")
		}
		b.WriteString(" " + entry.Message)
	}
}

// writeHandoff renders the latest handoff. td's `show` carries only that one,
// which is stated here rather than implied, so nobody reads a single handoff
// as the whole history of the work.
func writeHandoff(b *strings.Builder, h *handoff) {
	if h == nil {
		return
	}
	b.WriteString("\n\nLatest handoff (" + h.Session + ", " + h.Timestamp.UTC().Format(time.RFC3339) + "):")
	writeList(b, "done", h.Done)
	writeList(b, "remaining", h.Remaining)
	writeList(b, "decisions", h.Decisions)
	writeList(b, "uncertain", h.Uncertain)
}

func writeReviews(b *strings.Builder, reviews []review) {
	if len(reviews) == 0 {
		return
	}
	b.WriteString("\n\nReviews:")
	for _, r := range reviews {
		b.WriteString("\n- " + r.CreatedAt.UTC().Format(time.RFC3339) + " " + r.Decision)
		if r.ReviewerSession != "" {
			b.WriteString(" by " + r.ReviewerSession)
		}
		if r.SelfReview {
			b.WriteString(" (self-review)")
		}
		if r.Superseded {
			b.WriteString(" (superseded)")
		}
		if r.Summary != "" {
			b.WriteString(": " + r.Summary)
		}
	}
}

func writeSection(b *strings.Builder, heading, body string) {
	if body == "" {
		return
	}
	b.WriteString("\n\n")
	if heading != "" {
		b.WriteString(heading + ":\n")
	}
	b.WriteString(body)
}

func writeList(b *strings.Builder, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	b.WriteString("\n" + heading + ":")
	for _, item := range items {
		b.WriteString("\n- " + item)
	}
}

func writeField(b *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	b.WriteString("\n" + key + ": " + value)
}

func writeOptional(b *strings.Builder, key string, value *string) {
	if value != nil {
		writeField(b, key, *value)
	}
}

package tasks

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Expand retrieves the evidence behind a locator.
//
// The local part of a Tasks locator is the stable id and nothing else. Line
// numbers and title substrings are convenience references the CLI accepts;
// they move and they resolve ambiguously, so they are never locators here.
func (a *Adapter) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	if _, _, err := a.session(); err != nil {
		return recall.ExpandResponse{}, err
	}
	id := req.Locator.Local
	if !idPattern.MatchString(id) {
		return recall.ExpandResponse{}, fmt.Errorf("%w: %q is not a Tasks id", protocol.ErrLocatorUnknown, id)
	}

	rec, mark, err := a.fetchRecord(ctx, id)
	if err != nil {
		return recall.ExpandResponse{}, err
	}

	set, _, _ := a.config()
	content := render(rec, req.Detail, a.expansionContext(ctx, rec, req.Detail, set))
	truncated, boundary := false, ""
	if req.Budget > 0 && int64(len(content)) > req.Budget {
		content = truncate(content, int(req.Budget))
		truncated, boundary = true, "budget_bytes"
	}

	return recall.ExpandResponse{
		Content:            content,
		SourceRevision:     mark,
		Truncated:          truncated,
		TruncationBoundary: boundary,
		Provenance:         "tasks:" + rec.ID + " line " + fmt.Sprint(rec.Line) + " (" + rec.Store + ")",
	}, nil
}

// fetchRecord reads one task by id.
//
// `show` reaches the live file only, so a record swept to the archive falls
// back to a scan of the full listing. The fallback returns less — the archive
// listing carries no notes — but reporting locator_expired for a record that
// demonstrably still exists would be a false claim about the source, and the
// spec reserves that error for a source that changed incompatibly.
func (a *Adapter) fetchRecord(ctx context.Context, id string) (taskRecord, string, error) {
	args := []string{"show", id, "--json", "--include-done"}
	res, err := a.run(ctx, args...)
	if err != nil {
		return taskRecord{}, "", err
	}
	if res.ExitCode != exitNoMatch {
		var rec taskRecord
		if err := decodeJSON(res, &rec, args...); err != nil {
			return taskRecord{}, "", err
		}
		// The CLI resolves refs fuzzily. Returning a record it merely
		// considered similar would be exactly the "nearby record" that
		// docs/adapter-protocol.md forbids expansion from substituting.
		if rec.ID != id {
			return taskRecord{}, "", fmt.Errorf("%w: %s no longer names this record", protocol.ErrLocatorExpired, id)
		}
		return rec, watermark(res.Stdout), nil
	}

	listing := listArgs(ScopeAll, nil)
	listRes, err := a.run(ctx, listing...)
	if err != nil {
		return taskRecord{}, "", err
	}
	var records []taskRecord
	if err := decodeJSON(listRes, &records, listing...); err != nil {
		return taskRecord{}, "", err
	}
	for _, rec := range records {
		if rec.ID == id {
			return rec, watermark(listRes.Stdout), nil
		}
	}
	return taskRecord{}, "", fmt.Errorf("%w: the store no longer holds %s", protocol.ErrLocatorExpired, id)
}

// expansionContext gathers the surrounding work for DetailContext. It is the
// one detail level that means something beyond the record itself, so it is the
// only one that costs extra invocations.
func (a *Adapter) expansionContext(ctx context.Context, rec taskRecord, detail recall.DetailLevel, set settings) []string {
	if detail != recall.DetailContext {
		return nil
	}
	projectArgs := []string{"projects", "--json"}
	projectRes, err := a.run(ctx, projectArgs...)
	if err != nil {
		return nil
	}
	var projects []projectRecord
	if err := decodeJSON(projectRes, &projects, projectArgs...); err != nil {
		return nil
	}
	var siblings []string
	for _, p := range projects {
		if !slices.Contains(p.TaskIDs, rec.ID) {
			continue
		}
		listing := listArgs(set.Scope, nil)
		listRes, err := a.run(ctx, listing...)
		if err != nil {
			return nil
		}
		var records []taskRecord
		if err := decodeJSON(listRes, &records, listing...); err != nil {
			return nil
		}
		for _, other := range records {
			if other.ID != rec.ID && slices.Contains(p.TaskIDs, other.ID) {
				siblings = append(siblings, other.Headline)
			}
		}
		break
	}
	return siblings
}

// render turns a record into evidence text at one detail level.
//
// The levels widen rather than reshape: each one's output starts with the
// previous one's, so a caller comparing a summary against a full expansion
// sees added lines, not rewritten ones. Notes are the boundary between
// summary and the rest, because notes are where a task stops being a
// structured row and becomes prose.
func render(rec taskRecord, detail recall.DetailLevel, siblings []string) string {
	var b strings.Builder
	if rec.Headline != "" {
		b.WriteString(rec.Headline)
	} else {
		b.WriteString(rec.State + " " + rec.Title)
	}
	writeField(&b, "id", rec.ID)
	if rec.Project != nil {
		writeField(&b, "project", *rec.Project)
	}
	writeOptional(&b, "deadline", rec.Deadline)
	writeOptional(&b, "scheduled", rec.Scheduled)
	writeOptional(&b, "recur", rec.Recur)
	writeOptional(&b, "closed", rec.Closed)
	if !rec.Available {
		writeField(&b, "unavailable", rec.AvailabilityReason)
	}

	notes := rec.Notes
	switch detail {
	case recall.DetailSummary:
		notes = nil
	case recall.DetailExcerpt:
		if len(notes) > 1 {
			notes = notes[:1]
		}
	case recall.DetailFull, recall.DetailContext:
	default:
		// An unknown level is not an invitation to guess how much to reveal.
		notes = nil
	}
	for _, note := range notes {
		b.WriteString("\n\n")
		b.WriteString(note)
	}

	if detail == recall.DetailFull || detail == recall.DetailContext {
		for _, l := range rec.Links {
			b.WriteString("\n" + l.System + ": " + l.URL)
		}
	}
	if len(siblings) > 0 {
		b.WriteString("\n\nAlongside in this project:")
		for _, s := range siblings {
			b.WriteString("\n- " + s)
		}
	}
	return b.String()
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

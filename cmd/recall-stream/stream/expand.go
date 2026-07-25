package stream

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// episodeWindow is how far either side of a record DetailContext gathers.
// Adjacent events in a stream are an episode; the window is what makes that a
// bounded claim rather than "everything nearby".
const episodeWindow = 15 * time.Minute

// contextExcerptBytes bounds the preview of each neighbouring event.
const contextExcerptBytes = 120

// Expand retrieves the evidence behind a locator.
//
// The local part is "v<schema>/<id>". The schema version is part of the
// reference because it is part of what the reference promised: a record
// rewritten into another version is not the same evidence, and returning it
// would be the "different revision" the protocol forbids expansion from
// silently substituting.
func (a *Adapter) Expand(_ context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	if _, _, _, err := a.session(); err != nil {
		return recall.ExpandResponse{}, err
	}
	schema, id, err := parseLocal(req.Locator.Local)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	snap, err := a.current(false)
	if err != nil {
		return recall.ExpandResponse{}, err
	}

	rec, ok := snap.byID[req.Locator.Local]
	if !ok {
		// The record identity is (schema, id), so a miss here is either a
		// record the stream no longer holds — an append-only stream does not
		// lose records, so the file was rewritten — or a reference minted
		// against a shape this record has moved on from. Both are the same
		// incompatible change from the caller's side.
		if other, exists := anyShape(snap, id); exists {
			return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired,
				"%s is now schema v%d, the locator names v%d", id, other.schema, schema)
		}
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired,
			"%s holds no record %s", snap.generation(), id)
	}

	content := render(rec, req.Detail, a.episode(snap, rec, req.Detail))
	truncated, boundary := false, ""
	if req.Budget > 0 && int64(len(content)) > req.Budget {
		content = clipBytes(content, int(req.Budget))
		truncated, boundary = true, "budget_bytes"
	}
	return recall.ExpandResponse{
		Content:            content,
		SourceRevision:     snap.generation(),
		Truncated:          truncated,
		TruncationBoundary: boundary,
		Provenance:         fmt.Sprintf("%s@%d (%s)", rec.file, rec.offset, rec.local()),
	}, nil
}

// parseLocal reads the adapter-local part of a locator. A reference this
// adapter cannot read is locator_unknown, which is a different fact from a
// reference it can read that no longer resolves.
func parseLocal(local string) (int, string, error) {
	version, id, found := strings.Cut(local, "/")
	if !found || !strings.HasPrefix(version, "v") || id == "" {
		return 0, "", protocol.Errorf(protocol.CodeLocatorUnknown,
			"%q is not a stream locator, want v<schema>/<id>", local)
	}
	schema, err := strconv.Atoi(strings.TrimPrefix(version, "v"))
	if err != nil || schema < 1 {
		return 0, "", protocol.Errorf(protocol.CodeLocatorUnknown,
			"%q names no schema version", local)
	}
	return schema, id, nil
}

// render turns a record into evidence at one detail level.
//
// The levels widen rather than reshape: each one's output starts with the
// previous one's, so a caller comparing a summary against a full expansion
// sees added lines and not rewritten ones.
func render(rec record, detail recall.DetailLevel, episode []record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s at %s", rec.title, rec.kind, rec.eventTime.Format(time.RFC3339))
	if rec.system != "" && rec.ref != "" {
		fmt.Fprintf(&b, "\nupstream: %s %s", rec.system, rec.ref)
	}

	switch detail {
	case recall.DetailSummary:
		return b.String()
	case recall.DetailExcerpt:
		writeBody(&b, clip(rec.text, excerptBytes))
		return b.String()
	case recall.DetailFull, recall.DetailContext:
		writeBody(&b, rec.text)
	default:
		// An unknown level is not an invitation to guess how much to reveal.
		return b.String()
	}

	if rec.actor != "" {
		fmt.Fprintf(&b, "\nactor: %s", rec.actor)
	}
	if rec.correlation != "" {
		fmt.Fprintf(&b, "\ncorrelation: %s", rec.correlation)
	}
	if rec.revision != "" {
		fmt.Fprintf(&b, "\nupstream_revision: %s", rec.revision)
	}
	if len(episode) > 0 {
		b.WriteString("\n\nEpisode:")
		for _, other := range episode {
			// Each neighbour keeps its own locator. Grouping events into an
			// episode must not cost a reader the ability to expand one.
			fmt.Fprintf(&b, "\n- %s %s %s", other.local(),
				other.eventTime.Format(time.RFC3339), clip(other.title, contextExcerptBytes))
		}
	}
	return b.String()
}

func writeBody(b *strings.Builder, text string) {
	if text != "" {
		b.WriteString("\n\n")
		b.WriteString(text)
	}
}

// episode gathers the events adjacent in time to rec, in stream order.
func (a *Adapter) episode(snap *snapshot, rec record, detail recall.DetailLevel) []record {
	if detail != recall.DetailContext {
		return nil
	}
	var out []record
	for _, other := range snap.records {
		if other.id == rec.id {
			continue
		}
		if delta := other.eventTime.Sub(rec.eventTime); delta < -episodeWindow || delta > episodeWindow {
			continue
		}
		out = append(out, other)
	}
	return out
}

// clipBytes cuts at a rune boundary so a truncated expansion is still text.
func clipBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// anyShape finds a record with this id under any schema version, so a locator
// that named a shape the record has moved on from can say so rather than
// reporting the record missing.
func anyShape(snap *snapshot, id string) (record, bool) {
	for _, r := range snap.records {
		if r.id == id {
			return r, true
		}
	}
	return record{}, false
}

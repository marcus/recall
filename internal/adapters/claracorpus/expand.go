package claracorpus

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// contextLimit bounds how many neighbours a context expansion gathers. Context
// is a bounded claim about what sits beside a record, not everything the store
// happens to hold about it.
const contextLimit = 12

// Expand retrieves the evidence behind a locator.
//
// The local part is "<store>/v<schema>/<id>". The schema version is part of the
// reference because it is part of what the reference promised: Clara migrates a
// record between versions in place, and a record rewritten into another version
// is not the same evidence. Returning it would be the "different revision" the
// protocol forbids expansion from silently substituting.
func (a *Adapter) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	s, err := a.session()
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	prefix, schema, id, err := parseLocal(req.Locator.Local)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	if prefix != s.store.prefix() {
		// This instance serves one store. A locator naming the other one is not
		// a record that expired here — it never lived here — so it is unknown
		// rather than expired, and refusing it is what keeps a memory id from
		// being answered out of the signal stream.
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorUnknown,
			"%q names the %s store; this source serves %s",
			req.Locator.Local, storeOfPrefix(prefix), s.store)
	}
	snap, err := a.current(ctx, false)
	if err != nil {
		return recall.ExpandResponse{}, err
	}

	idx, ok := snap.byLocal[req.Locator.Local]
	if !ok {
		// A miss is either a record the corpus no longer holds — Clara's
		// consolidate and forget both delete — or a reference minted against a
		// shape the record has moved on from. Both are the same incompatible
		// change from the caller's side.
		if other, exists := anyShape(snap, id); exists {
			return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired,
				"%s is now schema v%d, the locator names v%d", id, other, schema)
		}
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired,
			"%s holds no record %s", snap.generation(), id)
	}
	it := &snap.items[idx]

	content := render(it, req.Detail, snap, s)
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
		Provenance:         fmt.Sprintf("%s (%s)", it.file(s.store), it.local),
	}, nil
}

func (k storeKind) prefix() string {
	if k == StoreMemory {
		return "mem"
	}
	return "sig"
}

func storeOfPrefix(prefix string) string {
	if prefix == "mem" {
		return string(StoreMemory)
	}
	return string(StoreSignals)
}

// file names the store file this record was read from, for provenance. Base
// names only: an expansion carries no local paths.
func (it *item) file(store storeKind) string {
	archived := (it.mem != nil && it.mem.archived) || (it.sig != nil && it.sig.archived)
	switch {
	case store == StoreMemory && archived:
		return fileMemoryArchive
	case store == StoreMemory:
		return fileMemory
	case archived:
		return fileSignalsArchive
	default:
		return fileSignals
	}
}

// parseLocal reads the adapter-local part of a locator. A reference this
// adapter cannot read at all is locator_unknown, which is a different fact from
// a reference it can read that no longer resolves.
func parseLocal(local string) (prefix string, schema int, id string, err error) {
	parts := strings.SplitN(local, "/", 3)
	if len(parts) != 3 || parts[2] == "" {
		return "", 0, "", protocol.Errorf(protocol.CodeLocatorUnknown,
			"%q is not a clara-corpus locator, want <mem|sig>/v<schema>/<id>", local)
	}
	if parts[0] != "mem" && parts[0] != "sig" {
		return "", 0, "", protocol.Errorf(protocol.CodeLocatorUnknown,
			"%q names no Clara store, want mem or sig", local)
	}
	if !strings.HasPrefix(parts[1], "v") {
		return "", 0, "", protocol.Errorf(protocol.CodeLocatorUnknown,
			"%q names no schema version", local)
	}
	version, convErr := strconv.Atoi(strings.TrimPrefix(parts[1], "v"))
	if convErr != nil || version < 1 {
		return "", 0, "", protocol.Errorf(protocol.CodeLocatorUnknown,
			"%q names no schema version", local)
	}
	return parts[0], version, parts[2], nil
}

// anyShape finds a record with this id under any schema version, so a locator
// naming a shape the record has moved on from can say so rather than reporting
// the record missing.
func anyShape(snap *snapshot, id string) (int, bool) {
	for i := range snap.items {
		it := &snap.items[i]
		if it.id != id {
			continue
		}
		if it.mem != nil {
			return it.mem.schema, true
		}
		return it.sig.schema, true
	}
	return 0, false
}

// render turns a record into evidence at one detail level.
//
// The levels widen rather than reshape: each one's output starts with the
// previous one's, so a caller comparing a summary against a full expansion sees
// added lines and not rewritten ones. Every value is written as a labelled
// field and sanitized first — see [oneLine] — because retrieved content is data
// and a body that could forge a section header would be read as this adapter's
// own words.
func render(it *item, detail recall.DetailLevel, snap *snapshot, s session) string {
	var b strings.Builder
	if it.mem != nil {
		renderMemory(&b, it, detail, snap, s)
	} else {
		renderSignal(&b, it, detail, snap, s)
	}
	return b.String()
}

func renderMemory(b *strings.Builder, it *item, detail recall.DetailLevel, snap *snapshot, s session) {
	r := it.mem
	fmt.Fprintf(b, "%s\nmemory %s · subject %s · %s",
		it.title, oneLine(r.kind), oneLine(r.subject), it.standing)
	// The decay calculation belongs in the summary, not below the body: it is
	// the difference between "Marcus prefers this" and "Marcus preferred this
	// two years ago", and a reader who stops at the first line still needs it.
	fmt.Fprintf(b, "\ndecay: %s", it.dec.explain())

	switch detail {
	case recall.DetailSummary:
		return
	case recall.DetailExcerpt:
		writeBody(b, clip(oneLine(r.body), excerptBytes))
		return
	case recall.DetailFull, recall.DetailContext:
		writeBody(b, oneLine(r.body))
	default:
		// An unknown level is not an invitation to guess how much to reveal.
		return
	}

	fmt.Fprintf(b, "\n\ncreated: %s", r.created)
	fmt.Fprintf(b, "\nlast_seen: %s (reinforced %d times)", r.lastSeen, r.hits)
	fmt.Fprintf(b, "\nweight: %v", round(r.weight, 4))
	if r.halfLife != nil {
		fmt.Fprintf(b, "\nhalf_life_days: %v", round(*r.halfLife, 6))
	} else {
		b.WriteString("\nhalf_life_days: null (does not decay)")
	}
	if len(r.tags) > 0 {
		fmt.Fprintf(b, "\ntags: %s", oneLine(strings.Join(r.tags, ", ")))
	}
	if len(r.links) > 0 {
		fmt.Fprintf(b, "\nlinks: %s", oneLine(strings.Join(r.links, ", ")))
	}
	if r.source != "" {
		fmt.Fprintf(b, "\nmemory_source: %s", oneLine(r.source))
	}
	if r.disabled {
		b.WriteString("\ndisabled: the owner removed this preference's effect; the record and its provenance stay")
	}
	if r.generated() {
		fmt.Fprintf(b, "\ngenerated preference: %s/%s direction %d, from %d observations (threshold %d)",
			oneLine(r.effectSource), oneLine(r.effectKind), r.effectDirection,
			len(r.provenanceRefs), r.threshold)
		for _, ref := range r.provenanceRefs {
			fmt.Fprintf(b, "\n- %s", oneLine(ref))
		}
	}
	if detail == recall.DetailContext {
		writeSubjectHistory(b, it, snap, s)
	}
}

// writeSubjectHistory gathers the other records sharing this memory's subject.
//
// A subject is Clara's stable topic key, and the same subject can hold a live
// record and one or more archived predecessors — which is exactly the history a
// question about a changed preference needs. Each neighbour keeps its own
// locator: grouping records into a history must not cost a reader the ability
// to expand one.
func writeSubjectHistory(b *strings.Builder, it *item, snap *snapshot, s session) {
	var lines []string
	for i := range snap.items {
		other := &snap.items[i]
		if other.local == it.local || other.mem == nil || other.mem.subject != it.mem.subject {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s:%s %s [%s] %s",
			s.sourceID, other.local, other.mem.lastSeen, other.standing,
			clip(oneLine(other.mem.body), 120)))
		if len(lines) == contextLimit {
			break
		}
	}
	if len(lines) == 0 {
		return
	}
	b.WriteString("\n\nSubject history:\n")
	b.WriteString(strings.Join(lines, "\n"))
}

func renderSignal(b *strings.Builder, it *item, detail recall.DetailLevel, snap *snapshot, s session) {
	r := it.sig
	fmt.Fprintf(b, "%s\n%s %s · %s", it.title, oneLine(r.source), oneLine(r.kind), it.standing)
	if !it.eventTime.IsZero() {
		fmt.Fprintf(b, " · %s", it.eventTime.Format(time.RFC3339))
	}
	fmt.Fprintf(b, "\nupstream: %s", oneLine(r.ref))
	if r.lastAction != "" {
		// The observation projection, at every detail level. What the owner did
		// about a signal changes what the signal means, and a summary that
		// omitted it would read as an open item.
		fmt.Fprintf(b, "\nlast_action: %s at %s",
			oneLine(r.lastAction), r.lastActionAt.Format(time.RFC3339))
	}

	switch detail {
	case recall.DetailSummary:
		return
	case recall.DetailExcerpt:
		writeBody(b, clip(oneLine(firstNonEmpty(r.summary, r.rawExcerpt)), excerptBytes))
		return
	case recall.DetailFull, recall.DetailContext:
		writeBody(b, oneLine(firstNonEmpty(r.summary, r.rawExcerpt)))
	default:
		return
	}

	writeField(b, "status", r.status)
	writeField(b, "priority", r.priority)
	writeField(b, "assignee", r.assignee)
	writeField(b, "requester", r.requester)
	writeField(b, "direct_kind", r.directKind)
	writeField(b, "due", r.due.String())
	writeField(b, "url", r.url)
	if len(r.people) > 0 {
		writeField(b, "people", strings.Join(r.people, ", "))
	}
	if r.rawExcerpt != "" && r.rawExcerpt != r.summary {
		writeField(b, "raw_excerpt", r.rawExcerpt)
	}
	writeField(b, "content_trust", r.contentTrust)
	fmt.Fprintf(b, "\nfirst_seen: %s, last_seen: %s, seen in %d runs",
		r.firstSeen, r.lastSeen, r.runCount)
	writeField(b, "last_confirmed", r.lastConfirmed.String())
	writeField(b, "lifecycle_state", r.lifecycleState)
	writeField(b, "inactive_reason", r.inactiveReason)
	writeField(b, "archived_at", r.archivedAt.String())

	if detail == recall.DetailContext {
		writeReactions(b, r, snap)
	}
}

// writeReactions renders the observation log for this signal.
//
// This is where observations become evidence a reader can see. They are never
// candidates — see the package doc — so a context expansion is the one place
// the full reaction history, with each reaction's own instant, is available.
func writeReactions(b *strings.Builder, r *sigRecord, snap *snapshot) {
	history := snap.byRef[r.ref]
	if len(history) == 0 {
		return
	}
	b.WriteString("\n\nReactions:")
	for i, o := range history {
		if i == contextLimit {
			fmt.Fprintf(b, "\n- …%d earlier reactions not shown", len(history)-contextLimit)
			break
		}
		fmt.Fprintf(b, "\n- %s", o.describe())
	}
}

func writeBody(b *strings.Builder, body string) {
	if body != "" {
		b.WriteString("\n\n")
		b.WriteString(body)
	}
}

func writeField(b *strings.Builder, key, value string) {
	if v := oneLine(value); v != "" {
		fmt.Fprintf(b, "\n%s: %s", key, v)
	}
}

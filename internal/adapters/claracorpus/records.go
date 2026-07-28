package claracorpus

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/marcus/recall/internal/recall"
)

// generatedPreferenceType is the provenance type Clara stamps on a preference
// it derived from observations. It is the marker that makes a memory record a
// composite with parents rather than something someone wrote down.
const generatedPreferenceType = "observations-v1"

// memRecord is one line of memory.jsonl or memory-archive.jsonl.
type memRecord struct {
	schema   int
	id       string
	kind     string
	subject  string
	title    string
	body     string
	weight   float64
	halfLife *float64
	created  civilDate
	lastSeen civilDate
	hits     int
	tags     []string
	links    []string
	source   string
	disabled bool
	archived bool

	// effect and provenance exist only on generated preferences. provenance is
	// where a learned generalization records the observations it rests on,
	// which is what turns it into a composite for lineage.
	effectSource    string
	effectKind      string
	effectDirection int
	provenanceType  string
	provenanceRefs  []string
	threshold       int
}

type wireMemory struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Subject  string   `json:"subject"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Weight   *float64 `json:"weight"`
	HalfLife *float64 `json:"half_life_days"`
	Created  string   `json:"created"`
	LastSeen string   `json:"last_seen"`
	Hits     int      `json:"hits"`
	Tags     []string `json:"tags"`
	Links    []string `json:"links"`
	Source   string   `json:"source"`
	Disabled bool     `json:"disabled"`
	Effect   *struct {
		Source    string `json:"source"`
		Kind      string `json:"kind"`
		Direction int    `json:"direction"`
	} `json:"effect"`
	Provenance *struct {
		Type      string   `json:"type"`
		Threshold int      `json:"threshold"`
		Refs      []string `json:"refs"`
	} `json:"provenance"`
}

func parseMemory(raw []byte, version int, archived bool) (memRecord, error) {
	var w wireMemory
	if err := json.Unmarshal(raw, &w); err != nil {
		return memRecord{}, fmt.Errorf("not a memory record: %w", err)
	}
	if w.ID == "" || w.Subject == "" {
		return memRecord{}, errors.New("a memory record needs an id and a subject")
	}
	r := memRecord{
		schema: version, id: w.ID, kind: w.Kind, subject: w.Subject,
		title: w.Title, body: w.Body, halfLife: w.HalfLife,
		created: parseCivil(w.Created), lastSeen: parseCivil(w.LastSeen),
		hits: w.Hits, tags: w.Tags, links: w.Links, source: w.Source,
		disabled: w.Disabled, archived: archived,
	}
	if w.Weight != nil {
		r.weight = *w.Weight
	}
	if w.Effect != nil {
		r.effectSource, r.effectKind, r.effectDirection = w.Effect.Source, w.Effect.Kind, w.Effect.Direction
	}
	if w.Provenance != nil {
		r.provenanceType, r.threshold, r.provenanceRefs = w.Provenance.Type, w.Provenance.Threshold, w.Provenance.Refs
	}
	return r, nil
}

// generated reports whether this record is the observations projection rather
// than something the owner or an import wrote. Clara's own test is the same
// three conditions.
func (r memRecord) generated() bool {
	return r.kind == "preference" && r.provenanceType == generatedPreferenceType && len(r.provenanceRefs) > 0
}

func (r memRecord) local() string { return fmt.Sprintf("mem/v%d/%s", r.schema, r.id) }

// sigRecord is one line of signals.jsonl or signals-archive.jsonl, with the
// observation projection applied.
type sigRecord struct {
	schema         int
	id             string
	source         string
	kind           string
	ref            string
	sourceID       string
	occurrenceID   string
	contentTrust   string
	title          string
	url            string
	status         string
	priority       string
	assignee       string
	reporter       string
	requester      string
	people         []string
	isDirect       bool
	directKind     string
	occurredAt     time.Time
	createdAt      time.Time
	updatedAt      time.Time
	startsAt       time.Time
	endsAt         time.Time
	due            civilDate
	firstSeen      civilDate
	lastSeen       civilDate
	lastConfirmed  civilDate
	runCount       int
	lifecycleState string
	inactiveReason string
	inactiveAt     civilDate
	archivedAt     civilDate
	summary        string
	rawExcerpt     string
	archived       bool

	// Projected from observations.jsonl at read time, exactly as Clara's
	// Signals::Store does. A persisted value would be a second truth.
	lastAction   string
	lastActionAt time.Time
	actionCount  int
}

type wireSignal struct {
	ID             string     `json:"id"`
	Source         string     `json:"source"`
	Kind           string     `json:"kind"`
	Ref            string     `json:"ref"`
	SourceID       string     `json:"source_id"`
	OccurrenceID   string     `json:"occurrence_id"`
	ContentTrust   string     `json:"content_trust"`
	Title          string     `json:"title"`
	URL            string     `json:"url"`
	Status         string     `json:"status"`
	Priority       string     `json:"priority"`
	Assignee       string     `json:"assignee"`
	Reporter       string     `json:"reporter"`
	Requester      string     `json:"requester"`
	People         []string   `json:"people"`
	IsDirect       bool       `json:"is_direct"`
	DirectKind     string     `json:"direct_kind"`
	OccurredAt     *time.Time `json:"occurred_at"`
	CreatedAt      *time.Time `json:"created_at"`
	UpdatedAt      *time.Time `json:"updated_at"`
	StartsAt       *time.Time `json:"starts_at"`
	EndsAt         *time.Time `json:"ends_at"`
	Due            string     `json:"due"`
	FirstSeen      string     `json:"first_seen"`
	LastSeen       string     `json:"last_seen"`
	LastConfirmed  string     `json:"last_confirmed"`
	RunCount       int        `json:"run_count"`
	LifecycleState string     `json:"lifecycle_state"`
	InactiveReason string     `json:"inactive_reason"`
	InactiveAt     string     `json:"inactive_at"`
	ArchivedAt     string     `json:"archived_at"`
	Summary        string     `json:"summary"`
	RawExcerpt     string     `json:"raw_excerpt"`
}

func parseSignal(raw []byte, version int, archived bool, _ *time.Location) (sigRecord, error) {
	var w wireSignal
	if err := json.Unmarshal(raw, &w); err != nil {
		return sigRecord{}, fmt.Errorf("not a signal record: %w", err)
	}
	if w.ID == "" || w.Ref == "" || w.Source == "" {
		return sigRecord{}, errors.New("a signal needs an id, a source, and a ref")
	}
	r := sigRecord{
		schema: version, id: w.ID, source: w.Source, kind: w.Kind, ref: w.Ref,
		sourceID: w.SourceID, occurrenceID: w.OccurrenceID, contentTrust: w.ContentTrust,
		title: w.Title, url: w.URL, status: w.Status, priority: w.Priority,
		assignee: w.Assignee, reporter: w.Reporter, requester: w.Requester,
		people: w.People, isDirect: w.IsDirect, directKind: w.DirectKind,
		due: parseCivil(w.Due), firstSeen: parseCivil(w.FirstSeen),
		lastSeen: parseCivil(w.LastSeen), lastConfirmed: parseCivil(w.LastConfirmed),
		runCount: w.RunCount, lifecycleState: w.LifecycleState,
		inactiveReason: w.InactiveReason, inactiveAt: parseCivil(w.InactiveAt),
		archivedAt: parseCivil(w.ArchivedAt), summary: w.Summary,
		rawExcerpt: w.RawExcerpt, archived: archived,
	}
	for target, value := range map[*time.Time]*time.Time{
		&r.occurredAt: w.OccurredAt, &r.createdAt: w.CreatedAt, &r.updatedAt: w.UpdatedAt,
		&r.startsAt: w.StartsAt, &r.endsAt: w.EndsAt,
	} {
		if value != nil {
			*target = value.UTC()
		}
	}
	return r, nil
}

func (r sigRecord) local() string { return fmt.Sprintf("sig/v%d/%s", r.schema, r.id) }

// active reports Clara's lifecycle verdict. A record written before lifecycle
// existed carries no state, and Clara reads that as active.
func (r sigRecord) active() bool {
	return r.lifecycleState == "" || r.lifecycleState == "active"
}

// obsRecord is one line of observations.jsonl: an immutable record of what the
// owner did about a signal.
type obsRecord struct {
	schema     int
	id         string
	ref        string
	signalID   string
	action     string
	source     string
	kind       string
	occurredAt time.Time
	metadata   map[string]string
	eventKey   string
}

type wireObservation struct {
	ID         string            `json:"id"`
	Ref        string            `json:"ref"`
	SignalID   string            `json:"signal_id"`
	Action     string            `json:"action"`
	Source     string            `json:"source"`
	Kind       string            `json:"kind"`
	OccurredAt *time.Time        `json:"occurred_at"`
	Metadata   map[string]string `json:"metadata"`
	EventKey   string            `json:"event_key"`
}

func parseObservation(raw []byte, version int) (obsRecord, error) {
	var w wireObservation
	if err := json.Unmarshal(raw, &w); err != nil {
		return obsRecord{}, fmt.Errorf("not an observation: %w", err)
	}
	if w.ID == "" || w.Ref == "" || w.Action == "" || w.OccurredAt == nil {
		return obsRecord{}, errors.New("an observation needs an id, a ref, an action, and occurred_at")
	}
	return obsRecord{
		schema: version, id: w.ID, ref: w.Ref, signalID: w.SignalID,
		action: w.Action, source: w.Source, kind: w.Kind,
		occurredAt: w.OccurredAt.UTC(), metadata: w.Metadata, eventKey: w.EventKey,
	}, nil
}

// describe renders one reaction as a line of evidence.
func (o obsRecord) describe() string {
	line := fmt.Sprintf("%s %s", o.occurredAt.Format(time.RFC3339), oneLine(o.action))
	if from, to := o.metadata["from_priority"], o.metadata["to_priority"]; from != "" && to != "" {
		line += fmt.Sprintf(" %s→%s", oneLine(from), oneLine(to))
	}
	if o.eventKey != "" {
		line += " (" + oneLine(o.eventKey) + ")"
	}
	return line
}

// recordTypeOf keeps a signal in the shape of what it projects.
//
// The Clara source decides, not the kind: `tasks` names its kinds after agenda
// states, so "waiting" and "someday" are both tasks, and a mapping keyed on
// kind would have to enumerate them. A source nobody here knows about is an
// event, which is the least committal of the five named types.
func recordTypeOf(source, kind string) recall.RecordType {
	switch strings.ToLower(source) {
	case "tasks", "jira":
		return recall.RecordTask
	case "slack", "outlook":
		return recall.RecordMessage
	case "calendar", "zoom":
		return recall.RecordEvent
	}
	switch strings.ToLower(kind) {
	case "todo", "task", "bug", "story":
		return recall.RecordTask
	case "dm", "mention", "thread", "email":
		return recall.RecordMessage
	default:
		return recall.RecordEvent
	}
}

// correspondence reports whether a signal's excerpt is other people's words.
//
// The source floor is `internal` because a signal about a task holds the same
// text the task holds, and the Tasks source is internal. That reasoning stops
// at correspondence: a Slack message, an email, a meeting invitation, or a
// transcript excerpt is a verbatim slice of somebody's private exchange, and it
// arrives here through a projection rather than through the source that owns
// it. Those candidates raise themselves, which an adapter may always do.
func correspondence(source string) bool {
	switch strings.ToLower(source) {
	case "slack", "outlook", "zoom", "calendar":
		return true
	default:
		return false
	}
}

// buildMemoryItems turns parsed memory records into indexed items.
func buildMemoryItems(snap *snapshot, records []memRecord, s session, at time.Time) {
	// The corpus's civil today, in the corpus's own zone — the same quantity
	// Clara::TZ.today produces, because the decay this reproduces is measured
	// in whole civil days and not in elapsed hours.
	today := civilOf(at.In(s.loc))
	if pinned := parseCivil(s.set.Today); !pinned.zero() {
		today = pinned
	}
	snap.today = today
	for _, r := range records {
		it := item{
			local: r.local(), id: r.id, recordType: RecordMemory,
			title:       oneLine(firstNonEmpty(r.title, r.subject)),
			excerpt:     clip(oneLine(r.body), excerptBytes),
			identifiers: []string{r.id, r.subject, r.local()},
			sensitivity: s.floor,
			mem:         &r,
			dec:         effectiveDecay(r.weight, r.halfLife, r.lastSeen, r.created, today),
			decays:      true,
		}
		switch {
		case r.archived:
			it.standing = standingArchived
		case r.disabled:
			// `preferences remove` keeps a generated record and its provenance
			// inspectable while disabling its effect. That is inactive, not
			// deleted, and the record stays retrievable.
			it.standing = standingInactive
		default:
			it.standing = standingLive
		}

		w := newWeigher()
		w.add(r.body, 0.4)
		w.add(r.kind, 0.5)
		w.add(strings.Join(r.links, " "), 0.5)
		w.add(strings.Join(r.tags, " "), 0.6)
		w.add(r.subject, 0.8)
		w.add(it.title, 1.0)
		it.weights, it.counts, it.length = w.weights, w.counts, w.length

		// A memory record asserts something from the day it was written. It has
		// no end: Clara archives a faded record rather than dating its death,
		// so there is no valid_to to state and inventing one would be a claim
		// the corpus does not make.
		if !r.created.zero() {
			created := r.created.at(s.loc)
			it.eventTime, it.validFrom = created, &created
		}

		meta := map[string]any{
			"schema":   r.schema,
			"standing": it.standing.String(),
			"hits":     r.hits,
		}
		text(meta, "kind", r.kind)
		text(meta, "subject", r.subject)
		text(meta, "memory_source", r.source)
		text(meta, "created", r.created.String())
		text(meta, "last_seen", r.lastSeen.String())
		strs(meta, "tags", r.tags)
		strs(meta, "links", r.links)
		it.dec.describe(meta)
		if r.generated() {
			meta["generated_preference"] = true
			meta["provenance_type"] = r.provenanceType
			meta["provenance_threshold"] = r.threshold
			meta["provenance_refs"] = len(r.provenanceRefs)
			text(meta, "effect_source", r.effectSource)
			text(meta, "effect_kind", r.effectKind)
			meta["effect_direction"] = r.effectDirection
			it.derived = memoryEdges(r, s)
		}
		if r.disabled {
			meta["disabled"] = true
		}
		it.metadata = meta
		it.fingerprint = memoryFingerprint(r, it)

		snap.items = append(snap.items, it)
		snap.count(it.standing)
		snap.observe(r.lastSeen, r.created)
	}
}

// memoryEdges names the observations a generated preference rests on.
//
// Every ref in the provenance belongs to one Clara source — the promotion scope
// is exactly source plus kind — so `effect.source` names it for all of them. A
// preference always rests on at least four refs, so the result is a composite:
// the core keeps its own lineage root and records the ancestors, which is what
// stops a learned generalization from being counted as evidence independent of
// the signals it generalizes.
func memoryEdges(r memRecord, s session) []recall.Locator {
	sourceID, ok := s.set.Upstream[r.effectSource]
	if !ok {
		// An unmapped source emits no edge. A learned preference then reads as
		// an independent unit, which slightly overcounts corroboration — and a
		// wrong lineage root would be worse.
		return nil
	}
	seen := map[string]bool{}
	var out []recall.Locator
	for _, ref := range r.provenanceRefs {
		local := nativeID("", ref)
		if local == "" || seen[local] {
			continue
		}
		seen[local] = true
		out = append(out, recall.Locator{SourceID: sourceID, Local: local})
	}
	return out
}

// buildSignalItems turns parsed signals into indexed items, projecting the
// observation log onto each one.
func buildSignalItems(snap *snapshot, records []sigRecord, s session) {
	for _, r := range records {
		if history := snap.byRef[r.ref]; len(history) > 0 {
			// Clara's projection: latest reaction per ref, ordered by
			// (occurred_at, id). The log is already in that order.
			latest := history[len(history)-1]
			r.lastAction, r.lastActionAt, r.actionCount = latest.action, latest.occurredAt, len(history)
			snap.withAction++
		}

		it := item{
			local: r.local(), id: r.id, recordType: recordTypeOf(r.source, r.kind),
			title:       oneLine(r.title),
			excerpt:     clip(oneLine(firstNonEmpty(r.summary, r.rawExcerpt)), excerptBytes),
			identifiers: []string{r.id, r.ref, r.sourceID, r.occurrenceID, r.local()},
			sensitivity: s.floor,
			sig:         &r,
			derived:     signalEdges(r, s),
		}
		if correspondence(r.source) {
			it.sensitivity = it.sensitivity.Raise(recall.SensitivityConfidential)
		}
		switch {
		case r.archived:
			it.standing = standingArchived
		case !r.active():
			it.standing = standingInactive
		default:
			it.standing = standingLive
		}

		w := newWeigher()
		w.add(r.rawExcerpt, 0.4)
		w.add(r.summary, 0.5)
		for _, field := range []string{r.source, r.kind, r.status, r.priority, r.assignee, r.requester, r.ref} {
			w.add(field, 0.5)
		}
		w.add(strings.Join(r.people, " "), 0.6)
		w.add(r.title, 1.0)
		it.weights, it.counts, it.length = w.weights, w.counts, w.length

		// The upstream event's own instant, which is the only time on a signal
		// that describes the world rather than Clara's bookkeeping.
		switch {
		case !r.occurredAt.IsZero():
			it.eventTime = r.occurredAt
		case !r.createdAt.IsZero():
			it.eventTime = r.createdAt
		case !r.firstSeen.zero():
			it.eventTime = r.firstSeen.at(s.loc)
		}
		// A signal is a claim that an upstream record needs attention. It held
		// from the day Clara first saw it until the day Clara retired it, which
		// is a validity window the corpus records rather than one invented here.
		if !r.firstSeen.zero() {
			from := r.firstSeen.at(s.loc)
			it.validFrom = &from
		}
		if end := firstDate(r.archivedAt, r.inactiveAt); !end.zero() {
			to := end.at(s.loc)
			it.validTo = &to
		}

		meta := map[string]any{
			"schema":    r.schema,
			"standing":  it.standing.String(),
			"is_direct": r.isDirect,
			"run_count": r.runCount,
		}
		text(meta, "clara_source", r.source)
		text(meta, "kind", r.kind)
		text(meta, "ref", r.ref)
		text(meta, "source_id", r.sourceID)
		text(meta, "occurrence_id", r.occurrenceID)
		// Carried through rather than reinterpreted: Clara marks every
		// source-derived signal untrusted, and so does Recall's own boundary.
		text(meta, "content_trust", r.contentTrust)
		text(meta, "status", r.status)
		text(meta, "priority", r.priority)
		text(meta, "assignee", r.assignee)
		text(meta, "requester", r.requester)
		text(meta, "direct_kind", r.directKind)
		text(meta, "url", r.url)
		text(meta, "due", r.due.String())
		text(meta, "first_seen", r.firstSeen.String())
		text(meta, "last_seen", r.lastSeen.String())
		text(meta, "last_confirmed", r.lastConfirmed.String())
		text(meta, "lifecycle_state", r.lifecycleState)
		text(meta, "inactive_reason", r.inactiveReason)
		strs(meta, "people", r.people)
		if r.lastAction != "" {
			// The observation projection. This is where "and then he dismissed
			// it" reaches a reader, without an observation ever becoming a
			// candidate of its own.
			text(meta, "last_action", r.lastAction)
			meta["last_action_at"] = r.lastActionAt.Format(time.RFC3339)
			meta["action_count"] = r.actionCount
		}
		it.metadata = meta
		it.fingerprint = signalFingerprint(r, it)

		snap.items = append(snap.items, it)
		snap.count(it.standing)
		snap.observe(r.lastSeen, r.firstSeen)
	}
}

// memoryFingerprint and signalFingerprint cover the complete semantic record
// plus every adapter-owned interpretation that can change a candidate. This is
// intentionally broader than revision metadata: body/title changes, lineage
// remapping, sensitivity raises, and decay changes must all stop two unlike
// candidates from collapsing as corroboration.
func memoryFingerprint(r memRecord, it item) string {
	return fingerprintValue(map[string]any{
		"schema": r.schema, "id": r.id, "kind": r.kind, "subject": r.subject,
		"title": r.title, "body": r.body, "weight": r.weight,
		"half_life_days": r.halfLife, "created": r.created.String(),
		"last_seen": r.lastSeen.String(), "hits": r.hits, "tags": r.tags,
		"links": r.links, "source": r.source, "disabled": r.disabled,
		"archived": r.archived, "effect_source": r.effectSource,
		"effect_kind": r.effectKind, "effect_direction": r.effectDirection,
		"provenance_type": r.provenanceType, "provenance_refs": r.provenanceRefs,
		"provenance_threshold": r.threshold, "derived_from": it.derived,
		"sensitivity": it.sensitivity, "standing": it.standing.String(),
		"effective_weight": it.dec.Effective, "age_days": it.dec.AgeDays,
		"decay_basis": it.dec.Basis,
	})
}

func signalFingerprint(r sigRecord, it item) string {
	return fingerprintValue(map[string]any{
		"schema": r.schema, "id": r.id, "source": r.source, "kind": r.kind,
		"ref": r.ref, "source_id": r.sourceID, "occurrence_id": r.occurrenceID,
		"content_trust": r.contentTrust, "title": r.title, "url": r.url,
		"status": r.status, "priority": r.priority, "assignee": r.assignee,
		"reporter": r.reporter, "requester": r.requester, "people": r.people,
		"is_direct": r.isDirect, "direct_kind": r.directKind,
		"occurred_at": r.occurredAt, "created_at": r.createdAt,
		"updated_at": r.updatedAt, "starts_at": r.startsAt, "ends_at": r.endsAt,
		"due": r.due.String(), "first_seen": r.firstSeen.String(),
		"last_seen": r.lastSeen.String(), "last_confirmed": r.lastConfirmed.String(),
		"run_count": r.runCount, "lifecycle_state": r.lifecycleState,
		"inactive_reason": r.inactiveReason, "inactive_at": r.inactiveAt.String(),
		"archived_at": r.archivedAt.String(), "summary": r.summary,
		"raw_excerpt": r.rawExcerpt, "archived": r.archived,
		"last_action": r.lastAction, "last_action_at": r.lastActionAt,
		"action_count": r.actionCount, "derived_from": it.derived,
		"sensitivity": it.sensitivity, "standing": it.standing.String(),
	})
}

// signalEdges names the upstream record this signal projects.
//
// The edge is the configured source_id plus the upstream system's own
// identifier, which is exactly the locator that source writes for the same
// record — so the projection and the original collapse into one lineage root
// and never corroborate each other. An unmapped Clara source yields no edge: an
// invented source_id would resolve somewhere, and a wrong lineage root is worse
// than a missing one.
func signalEdges(r sigRecord, s session) []recall.Locator {
	sourceID, ok := s.set.Upstream[r.source]
	if !ok {
		return nil
	}
	local := nativeID(r.sourceID, r.ref)
	if local == "" {
		return nil
	}
	return []recall.Locator{{SourceID: sourceID, Local: local}}
}

// nativeID is the upstream record's own identifier.
//
// Clara stores it twice: in `source_id`, which is the native immutable id, and
// inside `ref`, which is that id behind a source prefix. The native id is
// preferred; the ref is the fallback for a migrated record that predates the
// field. The prefix is stripped rather than kept because it is Clara's naming,
// not the upstream system's — a ref is "tasks:d7c7a8a8" while the Tasks adapter
// writes the locator "tasks:d7c7a8a8" out of source_id "d7c7a8a8", and only one
// of those two spellings is the local part.
func nativeID(sourceID, ref string) string {
	if sourceID != "" {
		return sourceID
	}
	if _, rest, found := strings.Cut(ref, ":"); found {
		return rest
	}
	return ""
}

func (s *snapshot) count(st standing) {
	if st == standingArchived {
		s.archived++
		return
	}
	s.live++
}

// observe advances the newest date any record carries, which is the closest
// thing these stores have to a revision.
func (s *snapshot) observe(dates ...civilDate) {
	for _, d := range dates {
		if d.after(s.latest) {
			s.latest = d
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstDate(dates ...civilDate) civilDate {
	for _, d := range dates {
		if !d.zero() {
			return d
		}
	}
	return civilDate{}
}

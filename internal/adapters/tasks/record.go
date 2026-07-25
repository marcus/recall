package tasks

import (
	"strings"
	"time"
)

// taskRecord is the Tasks CLI JSON task shape.
//
// One struct covers `list` and `show` because `show` is a strict superset:
// list omits Closed, Notes, Project, and Links. Unknown fields are ignored so
// a CLI that grows a field does not break retrieval — a structured source that
// refused to answer after an upstream release would be worse than one that
// carried one field less.
type taskRecord struct {
	ID       string   `json:"id"`
	State    string   `json:"state"`
	Priority *string  `json:"priority"`
	Title    string   `json:"title"`
	Tags     []string `json:"tags"`
	Contexts []string `json:"contexts"`
	Deferred bool     `json:"deferred"`

	Scheduled     *string   `json:"scheduled"`
	ScheduledTime *temporal `json:"scheduled_time"`
	Deadline      *string   `json:"deadline"`
	DeadlineTime  *temporal `json:"deadline_time"`
	Recur         *string   `json:"recur"`

	// Line is the physical position in the store. It is a convenience
	// reference, never a locator: it moves whenever anything above it moves.
	Line int `json:"line"`

	// Store is "live" or "archive". The CLI calls this field "source", which
	// collides with Recall's own meaning of the word, so it is renamed here.
	Store string `json:"source"`

	// Headline is the CLI's rendered one-line summary: state, priority, title,
	// tags, and contexts. It is the natural bounded excerpt for a task.
	Headline string `json:"headline"`

	Available             bool    `json:"available"`
	AvailabilityReason    string  `json:"availability_reason"`
	AvailabilityBlockerID *string `json:"availability_blocker_id"`
	AvailableAt           *string `json:"available_at"`

	// The rest arrive only from `show`.
	Closed  *string  `json:"closed"`
	Notes   []string `json:"notes"`
	Project *string  `json:"project"`
	Links   []link   `json:"links"`
}

// temporal is the CLI's time-of-day object. An all-day date carries none.
//
// The object also reports the local time, the stored zone, the fold, and the
// effective zone. None of them are decoded: Instant is the CLI's own
// resolution of all four, and re-deriving it here would mean reimplementing
// timezone rules that already have an owner — and disagreeing with it
// eventually.
type temporal struct {
	Instant *time.Time `json:"instant"`
}

type link struct {
	URL    string `json:"url"`
	System string `json:"system"`
}

// projectRecord is one element of `tasks projects --json`.
//
// TaskIDs is why this call is made at all: the bulk task shape carries no
// project, so it is the one cheap way to attach the routing field the spec
// requires structured sources to preserve. The rollup also reports counts and
// a stuck flag, which are a view over the project rather than a fact about any
// candidate, so they are left on the wire.
type projectRecord struct {
	Title   string   `json:"title"`
	TaskIDs []string `json:"task_ids"`
}

// checkReport is `tasks check --json`.
type checkReport struct {
	OK       bool  `json:"ok"`
	Errors   []any `json:"errors"`
	Warnings []any `json:"warnings"`
}

// valid reports whether a decoded record can be turned into a candidate. A
// record without an id has no locator and no lineage, so it is not a record.
func (r taskRecord) valid() bool { return idPattern.MatchString(r.ID) && r.Title != "" }

// eventTime is when the task's own dated boundary falls.
//
// Deadline wins over Scheduled: a deadline is when the work is due, a schedule
// is when it becomes available, and the former is what a temporal question
// about a task usually means. A timed value contributes its exact instant; an
// all-day value is anchored at UTC midnight, and the local date it came from
// stays verbatim in metadata so nothing depends on that anchoring.
func (r taskRecord) eventTime() *time.Time {
	if t := instantFor(r.Deadline, r.DeadlineTime); t != nil {
		return t
	}
	return instantFor(r.Scheduled, r.ScheduledTime)
}

func instantFor(date *string, at *temporal) *time.Time {
	if at != nil && at.Instant != nil {
		utc := at.Instant.UTC()
		return &utc
	}
	if date == nil || *date == "" {
		return nil
	}
	parsed, err := time.Parse(time.DateOnly, *date)
	if err != nil {
		return nil
	}
	return &parsed
}

// closedAt is when the record stopped being true, for DONE and CANCELLED
// tasks. Only `show` reports it, so a candidate built from a list has no
// valid_to even when it is closed; that is honest rather than inferred.
func (r taskRecord) closedAt() *time.Time {
	return instantFor(r.Closed, nil)
}

// metadata is the typed field block carried on every candidate.
//
// docs/spec.md requires structured sources to preserve typed fields rather
// than flattening a task into anonymous text, because ranking and routing
// downstream read them. Values keep their source types — booleans stay
// booleans, lists stay lists — and absent fields are omitted rather than
// written as empty strings, so "no deadline" and "deadline unknown" do not
// become the same value.
func (r taskRecord) metadata(project string) map[string]any {
	meta := map[string]any{
		"state":               r.State,
		"available":           r.Available,
		"availability_reason": r.AvailabilityReason,
		"deferred":            r.Deferred,
		"headline":            r.Headline,
		"store":               r.Store,
		"line":                r.Line,
	}
	putString(meta, "priority", r.Priority)
	putStrings(meta, "tags", r.Tags)
	putStrings(meta, "contexts", r.Contexts)
	putString(meta, "deadline", r.Deadline)
	putString(meta, "scheduled", r.Scheduled)
	putString(meta, "recur", r.Recur)
	putString(meta, "closed", r.Closed)
	putString(meta, "available_at", r.AvailableAt)
	putString(meta, "availability_blocker_id", r.AvailabilityBlockerID)
	if r.DeadlineTime != nil && r.DeadlineTime.Instant != nil {
		meta["deadline_at"] = r.DeadlineTime.Instant.UTC().Format(time.RFC3339)
	}
	if r.ScheduledTime != nil && r.ScheduledTime.Instant != nil {
		meta["scheduled_at"] = r.ScheduledTime.Instant.UTC().Format(time.RFC3339)
	}
	if project == "" && r.Project != nil {
		project = *r.Project
	}
	if project != "" {
		meta["project"] = project
	}
	if len(r.Links) > 0 {
		urls := make([]string, 0, len(r.Links))
		for _, l := range r.Links {
			urls = append(urls, l.URL)
		}
		meta["links"] = urls
	}
	return meta
}

func putString(meta map[string]any, key string, value *string) {
	if value != nil && *value != "" {
		meta[key] = *value
	}
}

func putStrings(meta map[string]any, key string, value []string) {
	if len(value) > 0 {
		meta[key] = value
	}
}

// fieldText is the record's own vocabulary — everything a `field` match may
// hit. The title is excluded because it is scored separately and more highly.
func (r taskRecord) fieldText(project string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(r.State))
	for _, t := range r.Tags {
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(t))
	}
	for _, c := range r.Contexts {
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(strings.TrimPrefix(c, "@")))
	}
	if project == "" && r.Project != nil {
		project = *r.Project
	}
	if project != "" {
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(project))
	}
	if r.Priority != nil {
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(*r.Priority))
	}
	return b.String()
}

// priorityOrder ranks the priority cookie for tie-breaking. An unset priority
// sorts last, which is the CLI's own convention.
func (r taskRecord) priorityOrder() int {
	if r.Priority == nil {
		return len("ABC") + 1
	}
	switch strings.ToUpper(*r.Priority) {
	case "A":
		return 0
	case "B":
		return 1
	case "C":
		return 2
	default:
		return len("ABC") + 1
	}
}

// projectIndex maps task id to project title for a whole store.
func projectIndex(projects []projectRecord) map[string]string {
	index := make(map[string]string)
	for _, p := range projects {
		for _, id := range p.TaskIDs {
			index[id] = p.Title
		}
	}
	return index
}

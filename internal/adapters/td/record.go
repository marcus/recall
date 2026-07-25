package td

import (
	"strings"
	"time"
)

// issue is the td issue shape.
//
// One struct covers `td list`, `td search`, and `td show` even though td does
// not emit one shape for all three: `list` and `search` marshal td's own issue
// model, while `show` builds a map by hand that drops a few fields (the
// creating session, the branch) and adds the record's history (logs, the
// latest handoff, recent reviews). Decoding both into one struct is safe
// because the names that appear in both mean the same thing; the fields only
// `show` fills are documented as such below, so nothing assumes a search
// candidate carries them.
//
// Unknown fields are ignored, so a td release that grows a field does not
// break retrieval. A structured source that refused to answer after an
// upstream release would be worse than one carrying one field less.
type issue struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Type        string   `json:"type"`
	Priority    string   `json:"priority"`
	Points      int      `json:"points"`
	Labels      []string `json:"labels"`
	Sprint      string   `json:"sprint"`

	// ParentID is the epic or parent task. td has no separate epic entity: an
	// epic is an issue with type "epic", and membership is this edge.
	ParentID string `json:"parent_id"`

	// Acceptance is the issue's acceptance criteria, kept separate from the
	// description by td and kept separate here for the same reason: it is the
	// definition of done, not more prose about the work.
	Acceptance string `json:"acceptance"`

	// The session fields are td's review state in denormalized form. They are
	// present in every shape, unlike ReviewHistory, which only `show` carries.
	CreatorSession           string `json:"creator_session"`
	ImplementerSession       string `json:"implementer_session"`
	ReviewerSession          string `json:"reviewer_session"`
	ReviewRequestedBySession string `json:"review_requested_by_session"`
	ClosedBySession          string `json:"closed_by_session"`

	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ReviewedAt *time.Time `json:"reviewed_at"`
	ClosedAt   *time.Time `json:"closed_at"`

	// Minor marks work td allows to bypass independent review.
	Minor bool `json:"minor"`

	CreatedBranch string `json:"created_branch"`

	// DeferUntil and DueDate are local calendar dates, not timestamps. td
	// stores them as `YYYY-MM-DD` strings and they stay strings here: turning
	// a date into an instant would invent a timezone td never stated.
	DeferUntil *string `json:"defer_until"`
	DueDate    *string `json:"due_date"`
	DeferCount int     `json:"defer_count"`

	// The rest arrive only from `td show`.
	Logs          []logEntry `json:"logs"`
	Handoff       *handoff   `json:"handoff"`
	ReviewHistory []review   `json:"review_history"`
}

// logEntry is one progress note on an issue. `td show` renames td's
// `session_id` to `session` and omits the log's own id, so this is the shape
// of the show payload and not of td's log model.
type logEntry struct {
	Message   string    `json:"message"`
	Type      string    `json:"type"`
	Session   string    `json:"session"`
	Timestamp time.Time `json:"timestamp"`
}

// handoff is the latest handoff recorded on an issue: what a session finished,
// what it left, what it decided, and what it was unsure about.
type handoff struct {
	Done      []string  `json:"done"`
	Remaining []string  `json:"remaining"`
	Decisions []string  `json:"decisions"`
	Uncertain []string  `json:"uncertain"`
	Session   string    `json:"session"`
	Timestamp time.Time `json:"timestamp"`
}

// review is one recorded review decision. `td show` returns the most recent
// few, newest last.
type review struct {
	ID              string    `json:"id"`
	Decision        string    `json:"decision"`
	Summary         string    `json:"summary"`
	ReviewerSession string    `json:"reviewer_session"`
	RequestedBy     string    `json:"requested_by"`
	SelfReview      bool      `json:"self_review"`
	Superseded      bool      `json:"superseded"`
	CreatedAt       time.Time `json:"created_at"`
}

// searchHit is one element of `td search --json`.
//
// The keys are capitalized because td marshals its internal result struct with
// no JSON tags. That is an accident of td's implementation rather than a
// stated contract, so it is pinned here in one place, with a fixture behind
// it, instead of being spread across the package.
type searchHit struct {
	Issue issue `json:"Issue"`

	// Score is td's own relevance bucket: 100 for an id equal to the query,
	// 90 for an id containing it, 80/70/60 for title equality, prefix, and
	// containment, 40 for a description match, 20 for a label match.
	Score int `json:"Score"`

	// MatchField is which field produced the score: id, title, description, or
	// labels.
	MatchField string `json:"MatchField"`
}

// workspaceInfo is `td info --json`.
//
// Only the fields this adapter can honestly use are decoded. Current td
// reports database as ".todos/issues.db"; newer or wrapped implementations may
// report an absolute database or root. The relative form is joined to the
// root independently resolved from the configured location, then checked
// against Project. An absolute form or Root is authoritative. current_session
// is absent because it changes between invocations.
type workspaceInfo struct {
	// Project is td's own name for the workspace: the base name of the
	// resolved root. It is reported alongside the configured workspace name in
	// diagnostics, so a location pointing somewhere unexpected is visible
	// rather than silent.
	Project  string `json:"project"`
	Database string `json:"database"`
	Root     string `json:"root"`

	Issues struct {
		Total      int64 `json:"total"`
		Open       int64 `json:"open"`
		InProgress int64 `json:"in_progress"`
		Blocked    int64 `json:"blocked"`
		InReview   int64 `json:"in_review"`
		Closed     int64 `json:"closed"`
	} `json:"issues"`
}

// fileLink is one element of `td files <id> --json`: a file a session declared
// this issue's work touched.
type fileLink struct {
	Path     string    `json:"file_path"`
	Role     string    `json:"role"`
	LinkedAt time.Time `json:"linked_at"`
}

// dependsOn is `td depends-on <id> --json`: the issues this one waits on.
type dependsOn struct {
	Dependencies []string `json:"dependencies"`
}

// dependents is `td blocked-by <id> --json`, which despite its name reports
// the issues WAITING ON this one — the downstream edge, not the upstream. The
// direct and transitive sets are reported separately because they are
// different facts: one is what those issues declare, the other is what the
// graph implies. Only the direct set is used, because the transitive set of a
// hub issue is most of a workspace and says little about this record.
type dependents struct {
	Direct []string `json:"direct"`
	All    []string `json:"all"`
}

// valid reports whether a decoded record can be turned into a candidate. A
// record without a well-formed id has no locator and no lineage, so it is not
// a record.
func (i issue) valid() bool { return idPattern.MatchString(i.ID) && i.Title != "" }

// eventTime is when this issue came into being.
//
// Creation, not last update: event_time is when the underlying event happened,
// and an issue's event is that the work was raised. Its last edit is a fact
// about the record, and the record's own timestamps travel in metadata where
// they cannot be mistaken for the event.
func (i issue) eventTime() *time.Time {
	if i.CreatedAt.IsZero() {
		return nil
	}
	at := i.CreatedAt.UTC()
	return &at
}

// validFrom and validTo bound when "this is live engineering work" was true.
// An issue starts being true when it is created and stops when it is closed;
// an open issue has no end, which is not the same fact as an unknown one.
func (i issue) validFrom() *time.Time { return i.eventTime() }

func (i issue) validTo() *time.Time {
	if i.ClosedAt == nil || i.ClosedAt.IsZero() {
		return nil
	}
	at := i.ClosedAt.UTC()
	return &at
}

// priorityOrder ranks td's priority cookie for tie-breaking. P0 is critical
// and P4 is none; anything unrecognized sorts last rather than in the middle,
// so an unknown value never outranks a stated one.
func (i issue) priorityOrder() int {
	switch strings.ToUpper(i.Priority) {
	case "P0":
		return 0
	case "P1":
		return 1
	case "P2":
		return 2
	case "P3":
		return 3
	case "P4":
		return 4
	default:
		return 5
	}
}

// headline is the one-line rendering used as a title-adjacent summary. td has
// no equivalent of a rendered headline in its JSON, so this builds the same
// facts a person reads first: what state the work is in, how urgent, and what
// it is.
func (i issue) headline() string {
	var b strings.Builder
	b.WriteString("[" + i.Status + "]")
	if i.Priority != "" {
		b.WriteString(" " + i.Priority)
	}
	if i.Type != "" && i.Type != "task" {
		b.WriteString(" " + i.Type)
	}
	b.WriteString(" " + i.Title)
	return b.String()
}

// metadata is the typed field block carried on every candidate.
//
// docs/spec.md requires structured sources to preserve typed fields rather
// than flattening a record into anonymous text, because ranking and routing
// downstream read them. Values keep their source types — numbers stay numbers,
// lists stay lists — and absent fields are omitted rather than written as
// empty strings, so "no epic" and "epic unknown" do not become the same value.
//
// The workspace is in here, on every candidate, because this adapter serves
// many workspaces and a candidate that could not say which one it came from
// would make provenance a property of configuration rather than of evidence.
func (i issue) metadata(w workspace, hit *searchHit) map[string]any {
	meta := map[string]any{
		"workspace":      w.Name,
		"workspace_root": w.Root,
		"status":         i.Status,
		"type":           i.Type,
		"headline":       i.headline(),
	}
	putString(meta, "priority", i.Priority)
	if i.Points > 0 {
		meta["points"] = i.Points
	}
	if len(i.Labels) > 0 {
		meta["labels"] = i.Labels
	}
	putString(meta, "epic", i.ParentID)
	putString(meta, "sprint", i.Sprint)
	putString(meta, "created_branch", i.CreatedBranch)
	if i.Minor {
		meta["minor"] = true
	}
	if i.DeferCount > 0 {
		meta["defer_count"] = i.DeferCount
	}
	putStringPtr(meta, "due_date", i.DueDate)
	putStringPtr(meta, "defer_until", i.DeferUntil)

	// Review state, as far as any bulk shape carries it. The reviewer and the
	// requester are separate people by td's own rule that a reviewer may not
	// be the implementer, so they are separate fields rather than one
	// "reviewed by".
	putString(meta, "creator_session", i.CreatorSession)
	putString(meta, "implementer_session", i.ImplementerSession)
	putString(meta, "reviewer_session", i.ReviewerSession)
	putString(meta, "review_requested_by_session", i.ReviewRequestedBySession)
	putString(meta, "closed_by_session", i.ClosedBySession)
	putTime(meta, "created_at", &i.CreatedAt)
	putTime(meta, "updated_at", &i.UpdatedAt)
	putTime(meta, "reviewed_at", i.ReviewedAt)
	putTime(meta, "closed_at", i.ClosedAt)

	if hit != nil {
		// td's own score and the field it matched. They are diagnostics about
		// this retrieval, not facts about the issue, but they are what makes a
		// local ordering explainable to whoever reads the result.
		meta["td_score"] = hit.Score
		putString(meta, "td_match_field", hit.MatchField)
	}
	return meta
}

func putString(meta map[string]any, key, value string) {
	if value != "" {
		meta[key] = value
	}
}

func putStringPtr(meta map[string]any, key string, value *string) {
	if value != nil && *value != "" {
		meta[key] = *value
	}
}

func putTime(meta map[string]any, key string, value *time.Time) {
	if value != nil && !value.IsZero() {
		meta[key] = value.UTC().Format(time.RFC3339)
	}
}

// handles are the entity-shaped names an issue answers to: its labels, its
// epic, and the sessions that worked on it. An entity filter matches against
// these and nothing else, because they are the only fields in a td issue that
// name a thing rather than describe one.
func (i issue) handles() []string {
	out := make([]string, 0, len(i.Labels)+5)
	out = append(out, i.Labels...)
	for _, s := range []string{
		i.ParentID, i.CreatorSession, i.ImplementerSession,
		i.ReviewerSession, i.ReviewRequestedBySession,
	} {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

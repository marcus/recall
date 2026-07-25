package recall

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// SourceUID is a source instance's immutable identity. It is generated once at
// configuration time and never edited. Every persisted reference keys on it, so
// renaming a source cannot invalidate an evaluation pack or a saved locator.
type SourceUID string

// sourceSep separates the source part of a reference from the adapter-defined
// local part. Source names may not contain it; local parts may, so every split
// takes the first occurrence only.
const sourceSep = ":"

var (
	// ErrMalformedLocator means the text is not a source-scoped reference.
	ErrMalformedLocator = errors.New("malformed locator")
	// ErrUnresolvedLocator means a locator lacks the identity the requested
	// form needs. Resolution against a profile is the caller's job.
	ErrUnresolvedLocator = errors.New("unresolved locator")
)

// Locator is a stable, printable reference that retrieves a record or evidence
// range again.
//
// It has two forms of the same reference. Display form uses SourceID so
// locators stay readable and copy-pasteable ("tasks:td-f62256"). Persisted form
// uses SourceUID so stored references survive a rename. Only the owning adapter
// interprets Local.
//
// Locators are always written in display form, including by adapters, which
// receive their configured SourceID at handshake for exactly this reason. The
// core overwrites the source part of a candidate's own locator with the
// identity configuration assigned, so a forged prefix cannot make one source
// answer as another. Only a derived_from edge's prefix is taken at face value,
// and that edge is resolved against the profile and dropped when unknown.
type Locator struct {
	SourceID  string    `json:"source_id,omitempty"`
	SourceUID SourceUID `json:"source_uid,omitempty"`
	Local     string    `json:"local"`
}

// ParseLocator reads display form, "<source_id>:<local>". The local part may
// contain separators; only the first is structural.
func ParseLocator(s string) (Locator, error) {
	name, local, err := splitRef(s)
	if err != nil {
		return Locator{}, err
	}
	return Locator{SourceID: name, Local: local}, nil
}

// ParsePersistedLocator reads persisted form, "<source_uid>:<local>".
func ParsePersistedLocator(s string) (Locator, error) {
	name, local, err := splitRef(s)
	if err != nil {
		return Locator{}, err
	}
	return Locator{SourceUID: SourceUID(name), Local: local}, nil
}

func splitRef(s string) (name, local string, err error) {
	name, local, found := strings.Cut(s, sourceSep)
	switch {
	case !found:
		return "", "", fmt.Errorf("%w %q: want <source>%s<local>", ErrMalformedLocator, s, sourceSep)
	case name == "":
		return "", "", fmt.Errorf("%w %q: empty source", ErrMalformedLocator, s)
	case local == "":
		return "", "", fmt.Errorf("%w %q: empty local part", ErrMalformedLocator, s)
	}
	return name, local, nil
}

// String renders display form. It falls back to the immutable identity when no
// display name is known, so a locator is never printed as a bare local part
// that no adapter could interpret.
func (l Locator) String() string {
	switch {
	case l.SourceID != "":
		return l.SourceID + sourceSep + l.Local
	case l.SourceUID != "":
		return string(l.SourceUID) + sourceSep + l.Local
	default:
		return l.Local
	}
}

// Persist renders the form safe to store in judgments, telemetry, and saved
// references. It fails rather than silently emitting a renameable name.
func (l Locator) Persist() (string, error) {
	if l.SourceUID == "" {
		return "", fmt.Errorf("%w %q: no source_uid; resolve against a profile first",
			ErrUnresolvedLocator, l.String())
	}
	if l.Local == "" {
		return "", fmt.Errorf("%w: empty local part", ErrMalformedLocator)
	}
	return string(l.SourceUID) + sourceSep + l.Local, nil
}

// Resolved reports whether the locator carries both forms of identity.
func (l Locator) Resolved() bool {
	return l.SourceID != "" && l.SourceUID != "" && l.Local != ""
}

// Zero reports whether the locator names nothing.
func (l Locator) Zero() bool { return l == Locator{} }

// LineageRoot returns the lineage identity of this locator: its persisted form.
// Grouping keys on this value, so it must not vary with display naming.
func (l Locator) LineageRoot() (LineageRoot, error) {
	s, err := l.Persist()
	return LineageRoot(s), err
}

// MarshalJSON emits display form. Locators appear in results, and a result is
// something a person reads and pastes back; the structured identity travels in
// [Candidate.SourceUID] alongside it.
//
// A locator naming no source is refused rather than emitted. Its text would be
// a bare local part, which [ParseLocator] cannot read back, so serializing it
// would produce a value that looks like a locator and is not one.
func (l Locator) MarshalJSON() ([]byte, error) {
	if l.Zero() {
		return []byte(`""`), nil
	}
	if l.SourceID == "" && l.SourceUID == "" {
		return nil, fmt.Errorf("%w %q: names no source, so it could not be read back",
			ErrUnresolvedLocator, l.Local)
	}
	return json.Marshal(l.String())
}

func (l *Locator) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*l = Locator{}
		return nil
	}
	parsed, err := ParseLocator(s)
	if err != nil {
		return err
	}
	*l = parsed
	return nil
}

// LineageRoot identifies the original record a candidate projects, after
// declared derivation edges are followed. It is the deduplication key: two
// candidates sharing a root are one piece of evidence and never corroborate
// each other.
//
// Its text is a persisted-form locator, so it is stable across source renames.
type LineageRoot string

// Locator recovers the reference a lineage root was built from.
func (r LineageRoot) Locator() (Locator, error) {
	return ParsePersistedLocator(string(r))
}

func (r LineageRoot) String() string { return string(r) }

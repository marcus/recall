package stream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// Schema versions this build can read. A record outside the range is counted
// as failed rather than guessed at: a shape nobody has written a parser for is
// not evidence, and silently dropping it would make an incomplete index look
// complete.
const (
	MinSchema = 1
	MaxSchema = 2
)

// maxLineBytes bounds one record. A stream line longer than this is a runaway
// producer, not a big event.
const maxLineBytes = 1 << 20

// record is one parsed event. Field names are the adapter's own; nothing here
// crosses the wire except through a candidate or an expansion.
type record struct {
	schema      int
	id          string
	kind        recall.RecordType
	title       string
	text        string
	actor       string
	system      string // upstream system name, mapped to a source_id by settings
	ref         string // upstream record's native identifier
	correlation string
	revision    string
	eventTime   time.Time
	observedAt  time.Time
	file        string
	offset      int64

	// weights maps each searchable token to what a match on it earns.
	// Tokenizing at index time rather than per query is what an index is for:
	// a search then costs one map lookup per query term instead of retokenizing
	// every field of every record.
	weights map[string]float64
}

// identifies reports whether term is one of this record's stable identifiers,
// compared whole. An unbounded substring match never counts.
func (r record) identifies(term string) bool {
	for _, id := range []string{r.id, r.ref, r.correlation, r.local()} {
		if id != "" && strings.EqualFold(id, term) {
			return true
		}
	}
	return false
}

// locator returns the adapter-local part of this record's locator. The schema
// version is in it on purpose: it is what lets expansion notice that the
// record behind a printed locator has been rewritten into a shape the locator
// was never minted against.
func (r record) local() string { return fmt.Sprintf("v%d/%s", r.schema, r.id) }

// wireRecord is the JSONL line as written. Both supported versions decode into
// it and [parseRecord] decides which fields are real for which version, so a
// v2 field appearing in a v1 line is ignored rather than half-honored.
type wireRecord struct {
	Schema      int        `json:"schema"`
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	EventTime   time.Time  `json:"event_time"`
	ObservedAt  *time.Time `json:"observed_at"`
	System      string     `json:"system"`
	Ref         string     `json:"ref"`
	Correlation string     `json:"correlation"`
	Title       string     `json:"title"`
	Body        string     `json:"body"` // schema 1 spelling
	Text        string     `json:"text"` // schema 2 spelling
	Actor       string     `json:"actor"`
	Revision    string     `json:"revision"`
}

func parseRecord(line []byte) (record, error) {
	var w wireRecord
	if err := json.Unmarshal(line, &w); err != nil {
		return record{}, fmt.Errorf("not a JSON object: %w", err)
	}
	if w.Schema < MinSchema || w.Schema > MaxSchema {
		return record{}, fmt.Errorf("unsupported schema version %d", w.Schema)
	}
	if w.ID == "" || w.EventTime.IsZero() {
		return record{}, errors.New("record needs an id and an event_time")
	}
	r := record{
		schema:      w.Schema,
		id:          w.ID,
		kind:        recall.RecordEvent,
		title:       w.Title,
		system:      w.System,
		ref:         w.Ref,
		correlation: w.Correlation,
		revision:    w.Revision,
		eventTime:   w.EventTime.UTC(),
	}
	if w.Kind != "" {
		r.kind = recall.RecordType(w.Kind)
	}
	if w.ObservedAt != nil {
		r.observedAt = w.ObservedAt.UTC()
	}
	switch w.Schema {
	case 1:
		r.text = w.Body
	case 2:
		r.text = w.Text
		r.actor = w.Actor
	}
	// A token appearing in several fields keeps the strongest weight: a title
	// hit says more about the record than a body hit.
	r.weights = map[string]float64{}
	r.weigh(r.text, 0.4)
	for _, field := range []string{string(r.kind), r.system, r.actor, r.ref} {
		r.weigh(field, 0.5)
	}
	r.weigh(r.title, 1.0)
	return r, nil
}

func (r record) weigh(text string, weight float64) {
	for _, token := range tokenize(text) {
		if r.weights[token] < weight {
			r.weights[token] = weight
		}
	}
}

// fileCursor is how far one file has been consumed. Offset always lands on a
// newline: a trailing line with no terminator is a write in progress, and
// consuming it would index half an event and never revisit it.
type fileCursor struct {
	Path    string `json:"path"`
	Offset  int64  `json:"offset"`
	Records int    `json:"records"`
	Failed  int    `json:"failed"`
	Missing bool   `json:"missing"`
}

// checkpoint is the last successful boundary, persisted in the workdir.
type checkpoint struct {
	Generation int64        `json:"generation"`
	UpdatedAt  time.Time    `json:"updated_at"`
	Watermark  string       `json:"source_watermark"`
	Files      []fileCursor `json:"files"`
}

// snapshot is one published index generation. It is immutable once published:
// a build produces a new one and swaps the pointer, so a search in flight
// keeps reading the generation it started on.
type snapshot struct {
	gen       int64
	records   []record
	byID      map[string]record
	files     []fileCursor
	builtAt   time.Time
	latest    time.Time // newest event_time indexed
	bytes     int64
	failed    int
	missing   []string
	rewritten bool
}

func (s *snapshot) generation() string { return fmt.Sprintf("gen-%d", s.gen) }

// watermark is freshness evidence a caller can compare between two searches.
// The stream publishes no revision of its own, so the bytes consumed plus the
// newest event is the strongest honest statement available.
func (s *snapshot) watermark() string {
	latest := "-"
	if !s.latest.IsZero() {
		latest = s.latest.Format(time.RFC3339)
	}
	return fmt.Sprintf("bytes=%d records=%d latest=%s", s.bytes, len(s.records), latest)
}

func (s *snapshot) coverage() recall.IndexCoverage {
	if s.failed > 0 || len(s.missing) > 0 {
		return recall.IndexPartial
	}
	return recall.IndexComplete
}

// build produces the next generation.
//
// The incremental path reuses prev's records and reads only the bytes appended
// since its cursors. It is abandoned — and a full rebuild runs instead —
// whenever a file is shorter than the offset already consumed from it, because
// that means bytes were rewritten rather than appended and every offset behind
// it is now meaningless.
//
// prior is the previous process's cursors, from the workdir checkpoint. It is
// consulted only for that shrink check, never to skip bytes: this adapter's
// records live in memory, so a fresh process has to read the files whole
// however far a checkpoint says a dead one got.
func build(paths []string, prev *snapshot, prior []fileCursor, gen int64, full bool, at time.Time) (*snapshot, error) {
	cursors := map[string]fileCursor{}
	if prev != nil && !full {
		for _, c := range prev.files {
			cursors[c.Path] = c
		}
	}
	check := cursors
	if len(check) == 0 {
		check = make(map[string]fileCursor, len(prior))
		for _, c := range prior {
			check[c.Path] = c
		}
	}
	rewritten := shrank(paths, check)
	if rewritten {
		cursors = map[string]fileCursor{}
	}
	incremental := len(cursors) > 0

	next := &snapshot{gen: gen, builtAt: at, rewritten: rewritten}
	if incremental {
		next.records = append(next.records, prev.records...)
		next.latest = prev.latest
	}

	for _, path := range paths {
		cur := cursors[path]
		cur.Path = path
		if err := readInto(next, &cur); err != nil {
			return nil, err
		}
		next.bytes += cur.Offset
		next.failed += cur.Failed
		next.files = append(next.files, cur)
		if cur.Missing {
			next.missing = append(next.missing, filepath.Base(path))
		}
	}

	if len(next.missing) == len(paths) {
		// Every configured file is gone. This is not a stream with no events;
		// it is a source that cannot be read, and the two must never look the
		// same to the core. Base names only: diagnostics carry no local paths.
		//
		// The error is built from its code rather than by wrapping the
		// sentinel with %w. protocol.AsError recovers an *protocol.Error with
		// errors.As, which would find the sentinel and send its bare name,
		// dropping everything said after the colon. errors.Is still matches:
		// [protocol.Error.Is] compares codes.
		return nil, protocol.Errorf(protocol.CodeSourceUnavailable,
			"no configured stream file is readable (%s)", strings.Join(next.missing, ", "))
	}

	next.byID = make(map[string]record, len(next.records))
	for _, r := range next.records {
		// Last write wins. An append-only stream should not repeat an id, but
		// if it does the later line is the later statement about the record.
		next.byID[r.id] = r
	}
	return next, nil
}

// shrank reports whether any file is shorter than what has been consumed from
// it, or has vanished after having been read.
func shrank(paths []string, cursors map[string]fileCursor) bool {
	for _, path := range paths {
		cur, ok := cursors[path]
		if !ok || cur.Offset == 0 {
			continue
		}
		st, err := os.Stat(path)
		if err != nil || st.Size() < cur.Offset {
			return true
		}
	}
	return false
}

// readInto consumes the bytes after cur.Offset, appending parsed records to
// snap and advancing the cursor over complete lines only.
func readInto(snap *snapshot, cur *fileCursor) error {
	f, err := os.Open(cur.Path)
	if errors.Is(err, fs.ErrNotExist) {
		cur.Missing = true
		return nil
	}
	if err != nil {
		return protocol.Errorf(protocol.CodeSourceUnavailable, "%s is not readable", filepath.Base(cur.Path))
	}
	defer f.Close() //nolint:errcheck // read-only handle

	if _, err := f.Seek(cur.Offset, io.SeekStart); err != nil {
		return protocol.Errorf(protocol.CodeSourceUnavailable, "%s cannot be positioned", filepath.Base(cur.Path))
	}
	name := filepath.Base(cur.Path)
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			// No terminator: a partial write, or the end of the file. Either
			// way the bytes stay unconsumed and the next pass sees them whole.
			if !errors.Is(err, io.EOF) {
				return protocol.Errorf(protocol.CodeSourceUnavailable, "%s became unreadable", name)
			}
			return nil
		}
		offset := cur.Offset
		cur.Offset += int64(len(line))

		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if len(trimmed) > maxLineBytes {
			cur.Failed++
			continue
		}
		rec, err := parseRecord(trimmed)
		if err != nil {
			// One unreadable line degrades coverage; it never fails the scan.
			// A single bad append would otherwise take the whole source down.
			cur.Failed++
			continue
		}
		rec.file, rec.offset = name, offset
		if rec.observedAt.IsZero() {
			rec.observedAt = snap.builtAt
		}
		if rec.eventTime.After(snap.latest) {
			snap.latest = rec.eventTime
		}
		snap.records = append(snap.records, rec)
		cur.Records++
	}
}

// loadCheckpoint reads the last boundary. A missing or unreadable file is not
// an error: it means this workdir has published nothing yet, and the scan that
// follows will write one.
func loadCheckpoint(workdir string) checkpoint {
	var cp checkpoint
	raw, err := os.ReadFile(filepath.Join(workdir, checkpointFile))
	if err != nil {
		return cp
	}
	if err := json.Unmarshal(raw, &cp); err != nil {
		return checkpoint{}
	}
	return cp
}

// saveCheckpoint records a published generation, atomically. It runs after the
// generation is published, never before: a checkpoint ahead of the data it
// describes would let a later pass skip records nothing ever indexed.
func saveCheckpoint(workdir string, cp checkpoint) error {
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	tmp := filepath.Join(workdir, checkpointFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(workdir, checkpointFile))
}

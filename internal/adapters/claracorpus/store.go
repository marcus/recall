package claracorpus

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// Clara schema versions this build can read.
//
// Clara supports 0 (legacy, unversioned), 1, and 2, and calls 0 read-only
// legacy. A record outside this range is counted as failed rather than guessed
// at: content_trust, lifecycle_state, and deterministic observation identity
// only exist from version 2, and a shape nobody has written a parser for is not
// evidence. Counting it keeps an incomplete index from looking complete.
const (
	MinSchema = 1
	MaxSchema = 2
)

// maxLineBytes bounds one record. A JSONL line longer than this is a runaway
// producer, not a big memory.
const maxLineBytes = 1 << 20

// excerptBytes bounds a candidate's preview. A candidate is a pointer, not a
// payload; the locator is how a caller gets the rest.
const excerptBytes = 240

// fileRole is what a store file contributes.
type fileRole int

const (
	// roleLive is the store's current records. Absent means the corpus has not
	// written this store yet, which is a complete empty store and not a failure.
	roleLive fileRole = iota
	// roleArchive is the cold half of the same store: records Clara faded out,
	// still retrievable and ranked below live ones.
	roleArchive
	// roleObservations is the immutable behavioral log projected onto signals.
	roleObservations
)

type storeFile struct {
	path string
	role fileRole
}

// fileStamp is what change detection compares, and what the workdir checkpoint
// records. Size and modification time together are the whole of it: Clara
// rewrites these files in place, so there is no cursor to advance.
type fileStamp struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	Exists  bool      `json:"exists"`
	Records int       `json:"records"`
	Failed  int       `json:"failed"`
}

// checkpoint is the last successful boundary, persisted in the workdir.
type checkpoint struct {
	Generation int64       `json:"generation"`
	UpdatedAt  time.Time   `json:"updated_at"`
	Watermark  string      `json:"source_watermark"`
	Files      []fileStamp `json:"files"`
}

// standing is Clara's own verdict on how current a record is, carried through
// rather than recomputed. Live records outrank inactive ones, which outrank
// archived ones, and this adapter performs no age arithmetic to decide it: for
// signals the corpus already applied terminal statuses, per-source expiry, and
// archival, and re-deriving any of that would be a second lifecycle model.
type standing int

const (
	standingLive standing = iota
	standingInactive
	standingArchived
)

func (s standing) String() string {
	switch s {
	case standingInactive:
		return "inactive"
	case standingArchived:
		return "archived"
	default:
		return "live"
	}
}

// item is one indexed record, ready to be ranked and rendered. Everything that
// does not depend on the query is computed once, at index time.
type item struct {
	local       string // the locator's adapter-local part
	id          string
	recordType  recall.RecordType
	title       string
	excerpt     string
	identifiers []string
	weights     map[string]float64

	// counts and length back [recall.Candidate.Relevance]: how often each token
	// occurs in this record, and how long the record is. weights cannot answer
	// either — it keeps a token's strongest field weight and forgets the rest.
	counts map[string]int
	length int

	eventTime time.Time
	validFrom *time.Time
	validTo   *time.Time

	standing    standing
	dec         decay
	decays      bool
	derived     []recall.Locator
	sensitivity recall.Sensitivity
	fingerprint string
	metadata    map[string]any

	// Exactly one of these is set. Expansion needs the whole record; ranking
	// needs none of it.
	mem *memRecord
	sig *sigRecord
}

// snapshot is one published index generation. It is immutable once published: a
// build produces a new one and swaps the pointer, so a search in flight keeps
// reading the generation it started on.
type snapshot struct {
	gen     int64
	store   storeKind
	items   []item
	byLocal map[string]int
	obs     []obsRecord
	byRef   map[string][]obsRecord

	files   []fileStamp
	builtAt time.Time
	bytes   int64
	digest  string

	live       int
	archived   int
	withAction int
	failed     int
	obsFailed  int
	duplicates int
	absent     []string
	schemas    map[string]int
	latest     civilDate

	// today is the civil date memory records were aged from, for the record.
	// Zero for a signals store, which ages nothing.
	today civilDate
}

func (s *snapshot) generation() string { return fmt.Sprintf("gen-%d", s.gen) }

// holds reports whether any record in this snapshot uses a term.
//
// It is the store-wide membership test number-variant resolution needs: a term
// this store spells the caller's way is matched as written, and only a term it
// spells nowhere is looked for under another number. It scans rather than
// keeping an inverted index because a snapshot is a slice of records already in
// memory and a query carries a handful of terms — this is a map lookup per
// record per term, once per search, against a store whose whole point is that
// it is small.
func (s *snapshot) holds(term string) bool {
	for i := range s.items {
		if _, ok := s.items[i].weights[term]; ok {
			return true
		}
	}
	return false
}

// watermark is freshness evidence a caller can compare between two searches.
//
// Clara publishes no revision of its own, so the deterministic digest is the
// content identity and the counts/date make it inspectable. Deliberately no
// modification time: a checkout does not preserve one, and a watermark that
// changed with the filesystem rather than with the data would say nothing true.
func (s *snapshot) watermark() string {
	latest := "-"
	if !s.latest.zero() {
		latest = s.latest.String()
	}
	if s.store == StoreSignals {
		return fmt.Sprintf("live=%d archived=%d observations=%d bytes=%d latest=%s digest=%s",
			s.live, s.archived, len(s.obs), s.bytes, latest, s.digest)
	}
	return fmt.Sprintf("live=%d archived=%d bytes=%d latest=%s digest=%s",
		s.live, s.archived, s.bytes, latest, s.digest)
}

// coverage reports whether this generation represents the whole store.
//
// A line that failed to parse is a record that is unknown, not absent, and an
// observation that failed to parse leaves a signal's action state unknown. An
// absent optional file is neither: Clara writes each store on first use, so a
// corpus with no archive and no observations is complete.
func (s *snapshot) coverage() recall.IndexCoverage {
	if s.failed > 0 || s.obsFailed > 0 {
		return recall.IndexPartial
	}
	return recall.IndexComplete
}

// changed reports whether any store file differs from the stamps a generation
// was built from. A rewritten file changes size or modification time; a file
// that appeared or vanished changes existence.
func changed(files []storeFile, stamps []fileStamp) bool {
	byPath := make(map[string]fileStamp, len(stamps))
	for _, st := range stamps {
		byPath[st.Path] = st
	}
	if len(files) != len(stamps) {
		return true
	}
	for _, f := range files {
		prev, seen := byPath[f.path]
		if !seen {
			return true
		}
		info, err := os.Stat(f.path)
		if err != nil {
			if prev.Exists {
				return true
			}
			continue
		}
		if !prev.Exists || info.Size() != prev.Size || !info.ModTime().Equal(prev.ModTime) {
			return true
		}
	}
	return false
}

// build reads every store file and publishes the next generation.
//
// The whole store is reparsed every time, which is what makes deletion work:
// Clara's consolidate removes memory records and its lifecycle moves signals to
// the archive, and docs/spec.md#index-obligations requires a generation to
// exclude what the source no longer holds. An incremental cursor over these
// files would resurrect every one of them.
func build(ctx context.Context, files []storeFile, s session, gen int64, at time.Time) (*snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := ""
	if len(files) > 0 {
		dir = filepath.Dir(files[0].path)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		// The corpus directory went away after the handshake. Every file would
		// read as absent, and "the corpus is gone" must never look like "the
		// corpus is empty".
		return nil, protocol.Errorf(protocol.CodeSourceUnavailable,
			"clara-corpus: the corpus data directory is no longer readable")
	}

	next := &snapshot{
		gen: gen, store: s.store, builtAt: at,
		byLocal: map[string]int{}, byRef: map[string][]obsRecord{},
		schemas: map[string]int{},
	}

	var (
		mem []memRecord
		sig []sigRecord
	)
	content := sha256.New()
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, _ = io.WriteString(content, filepath.Base(f.path))
		_, _ = content.Write([]byte{0, byte(f.role), 0})
		stamp, err := scan(ctx, f, next, s, &mem, &sig, content)
		if err != nil {
			return nil, err
		}
		next.files = append(next.files, stamp)
		next.bytes += stamp.Size
		if !stamp.Exists {
			next.absent = append(next.absent, filepath.Base(f.path))
		}
	}
	next.digest = hex.EncodeToString(content.Sum(nil)[:16])

	sortObservations(next.obs)
	for _, o := range next.obs {
		next.byRef[o.ref] = append(next.byRef[o.ref], o)
	}

	switch s.store {
	case StoreMemory:
		mem, next.duplicates = dedupeMemory(mem)
		buildMemoryItems(next, mem, s, at)
	case StoreSignals:
		sig, next.duplicates = dedupeSignals(sig)
		buildSignalItems(next, sig, s)
	}
	for i, it := range next.items {
		next.byLocal[it.local] = i
	}
	return next, nil
}

// scan reads one file into the snapshot, returning the stamp that describes it.
func scan(
	ctx context.Context,
	f storeFile,
	snap *snapshot,
	s session,
	mem *[]memRecord,
	sig *[]sigRecord,
	digest hash.Hash,
) (fileStamp, error) {
	stamp := fileStamp{Path: f.path}
	file, err := os.Open(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		// Clara creates each store on first write. Absence inside a directory
		// that has already been proved to be a corpus means nothing has been
		// written there yet — a complete empty store, not a failure.
		_, _ = digest.Write([]byte("absent\x00"))
		return stamp, nil
	}
	if err != nil {
		return stamp, protocol.Errorf(protocol.CodeSourceUnavailable,
			"clara-corpus: %s is not readable", filepath.Base(f.path))
	}
	defer file.Close() //nolint:errcheck // read-only handle
	_, _ = digest.Write([]byte("present\x00"))

	if info, err := file.Stat(); err == nil {
		stamp.Size, stamp.ModTime = info.Size(), info.ModTime()
	}
	stamp.Exists = true

	name := filepath.Base(f.path)
	br := bufio.NewReader(file)
	for {
		if err := ctx.Err(); err != nil {
			return stamp, err
		}
		raw, tooLong, atEOF, err := readBoundedLine(ctx, br, digest)
		if err != nil {
			return stamp, protocol.Errorf(protocol.CodeSourceUnavailable,
				"clara-corpus: %s became unreadable", name)
		}
		if atEOF && len(raw) == 0 && !tooLong {
			return stamp, nil
		}
		trimmed := bytes.TrimSpace(raw)
		if tooLong {
			stamp.Failed++
			if f.role == roleObservations {
				snap.obsFailed++
			} else {
				snap.failed++
			}
		} else if len(trimmed) > 0 {
			if err := absorb(trimmed, f, snap, s, mem, sig); err != nil {
				// One unreadable line degrades coverage; it never fails the
				// scan. A single bad append would otherwise take a whole
				// corpus down.
				stamp.Failed++
				if f.role == roleObservations {
					snap.obsFailed++
				} else {
					snap.failed++
				}
			} else {
				stamp.Records++
			}
		}
		if atEOF {
			return stamp, nil
		}
	}
}

// readBoundedLine reads one JSONL record without ever allocating in proportion
// to an attacker-controlled line. ReadSlice exposes fixed-size reader
// fragments; once the record crosses the limit, the fragments are hashed and
// discarded until the newline. The digest still identifies the bad content,
// while coverage records the line as unknown.
func readBoundedLine(
	ctx context.Context,
	br *bufio.Reader,
	digest hash.Hash,
) (raw []byte, tooLong, atEOF bool, err error) {
	const bufferedLimit = maxLineBytes + 1 // one optional trailing newline
	for {
		if err := ctx.Err(); err != nil {
			return nil, tooLong, false, err
		}
		fragment, readErr := br.ReadSlice('\n')
		if len(fragment) > 0 {
			_, _ = digest.Write(fragment)
			if !tooLong {
				if len(raw)+len(fragment) > bufferedLimit {
					raw = nil
					tooLong = true
				} else {
					raw = append(raw, fragment...)
				}
			}
		}
		switch {
		case readErr == nil:
			return raw, tooLong, false, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			return raw, tooLong, true, nil
		default:
			return nil, tooLong, false, readErr
		}
	}
}

// absorb parses one line into the right collection.
func absorb(raw []byte, f storeFile, snap *snapshot, s session, mem *[]memRecord, sig *[]sigRecord) error {
	if len(raw) > maxLineBytes {
		return errors.New("line exceeds the record size limit")
	}
	version, kind, err := recordShape(raw)
	if err != nil {
		return err
	}
	if version < MinSchema || version > MaxSchema {
		return fmt.Errorf("unsupported schema version %d", version)
	}
	snap.schemas[fmt.Sprintf("v%d", version)]++

	if f.role == roleObservations {
		if kind != "observation" {
			return fmt.Errorf("observations.jsonl holds a %q record", kind)
		}
		o, err := parseObservation(raw, version)
		if err != nil {
			return err
		}
		snap.obs = append(snap.obs, o)
		return nil
	}

	archived := f.role == roleArchive
	switch s.store {
	case StoreMemory:
		if kind != "memory" {
			return fmt.Errorf("the memory store holds a %q record", kind)
		}
		r, err := parseMemory(raw, version, archived)
		if err != nil {
			return err
		}
		*mem = append(*mem, r)
	case StoreSignals:
		if kind != "signal" {
			return fmt.Errorf("the signal store holds a %q record", kind)
		}
		r, err := parseSignal(raw, version, archived, s.loc)
		if err != nil {
			return err
		}
		*sig = append(*sig, r)
	}
	return nil
}

// recordShape reads the two fields every Clara record carries before anything
// decides how to parse the rest.
func recordShape(raw []byte) (version int, kind string, err error) {
	var head struct {
		Type   string `json:"type"`
		Schema *int   `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return 0, "", fmt.Errorf("not a JSON object: %w", err)
	}
	if head.Type == "" {
		return 0, "", errors.New("record names no type")
	}
	if head.Schema == nil {
		// Clara calls an unversioned record legacy v0 and reads it only to
		// migrate it. Naming the version is what makes the locator honest.
		return 0, head.Type, nil
	}
	return *head.Schema, head.Type, nil
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

// saveCheckpoint makes a candidate generation durable before it is published.
// The temp file and directory are synced around the atomic rename, so success
// means a restart will advance past this identity even if the process exits
// before the in-memory pointer swap.
func saveCheckpoint(workdir string, cp checkpoint) error {
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(workdir, checkpointFile+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best-effort cleanup after any failure
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(workdir, checkpointFile)); err != nil {
		return err
	}
	dir, err := os.Open(workdir)
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // Sync below is the durability boundary
	return dir.Sync()
}

// fingerprint hashes a record's identity together with the fields that change
// when the record does.
//
// It is advisory, and it exists so that two instances accidentally configured
// over one store collapse for corroboration instead of agreeing with
// themselves. Deliberately not over the resolved directory, the store name, or
// the generation: those are precisely what two such instances would disagree
// about, and a fingerprint built on any of them would differ for the same
// record and defeat itself.
func fingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

func fingerprintValue(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		// Every value supplied by this package is composed of JSON-native
		// scalars, slices, maps, and timestamps. A future unsupported value is a
		// programmer defect; silently weakening identity would be worse.
		panic(fmt.Sprintf("clara-corpus fingerprint: %v", err))
	}
	return fingerprint(string(raw))
}

// dedupeMemory and dedupeSignals implement the corpus's repair-time
// last-write-wins rule before either search or expansion sees a record. Keeping
// both candidates while only byLocal kept the last made the two operations
// disagree about what one locator meant.
func dedupeMemory(in []memRecord) ([]memRecord, int) {
	seen := make(map[string]bool, len(in))
	out := make([]memRecord, 0, len(in))
	duplicates := 0
	for i := len(in) - 1; i >= 0; i-- {
		local := in[i].local()
		if seen[local] {
			duplicates++
			continue
		}
		seen[local] = true
		out = append(out, in[i])
	}
	slices.Reverse(out)
	return out, duplicates
}

func dedupeSignals(in []sigRecord) ([]sigRecord, int) {
	seen := make(map[string]bool, len(in))
	out := make([]sigRecord, 0, len(in))
	duplicates := 0
	for i := len(in) - 1; i >= 0; i-- {
		local := in[i].local()
		if seen[local] {
			duplicates++
			continue
		}
		seen[local] = true
		out = append(out, in[i])
	}
	slices.Reverse(out)
	return out, duplicates
}

// sortObservations orders the behavioral log the way Clara does: by occurrence
// instant, then by the stable event id. The id tie-break is what makes the
// projection independent of JSONL line order, so two corpora holding the same
// observations project the same last action.
func sortObservations(obs []obsRecord) {
	sort.SliceStable(obs, func(i, j int) bool {
		l, r := obs[i], obs[j]
		if !l.occurredAt.Equal(r.occurredAt) {
			return l.occurredAt.Before(r.occurredAt)
		}
		return l.id < r.id
	})
}

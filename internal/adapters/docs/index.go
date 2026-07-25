package docs

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/marcus/recall/internal/recall"
)

// On-disk layout under the handshake workdir:
//
//	<workdir>/index/current                    text: the published directory name
//	<workdir>/index/gen-000003-<digest>/       a published generation
//	<workdir>/index/build-<pid>-<nonce>/       a build in progress
//
// Publication is a single rename of the pointer file, performed only after the
// generation directory is complete and fsynced. A reader therefore observes
// either the previous generation or the new one, never a half-written index —
// which is why an interrupted build costs freshness and nothing else.
const (
	indexFormat  = 1
	indexFile    = "index.jsonl"
	currentFile  = "current"
	genPrefix    = "gen-"
	buildPrefix  = "build-"
	genDirPerm   = 0o700
	genFilePerm  = 0o600
	digestLength = 12
)

// errNoGeneration reports that nothing has been published yet. It is not an
// index failure: it is the state before the first build.
var errNoGeneration = errors.New("docs: no published index generation")

// indexFailure is one record the build could not index. It is retained in the
// generation so failed_count survives a restart and coverage can be reported
// without re-reading the corpus.
type indexFailure struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type indexedDoc struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Project string `json:"project,omitempty"`
	Size    int64  `json:"size"`
	// ModTime is the document's own event time: when it was last written.
	ModTime time.Time `json:"mod_time"`
	// Revision is a content hash, so expansion can tell whether the file moved
	// past the generation it was retrieved from.
	Revision    string   `json:"revision"`
	Identifiers []string `json:"identifiers,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
	ChunkCount  int      `json:"chunk_count"`
}

// RecordID is the document's identity. Every chunk of this file reports it, so
// the chunks collapse into one corroboration unit instead of pretending to be
// independent evidence.
func (d indexedDoc) RecordID() string { return d.Path }

type indexedChunk struct {
	Path        string         `json:"path"`
	Ord         int            `json:"ord"`
	StartLine   int            `json:"start_line"`
	EndLine     int            `json:"end_line"`
	Heading     string         `json:"heading,omitempty"`
	HeadingPath []string       `json:"heading_path,omitempty"`
	Excerpt     string         `json:"excerpt,omitempty"`
	Fingerprint string         `json:"fingerprint"`
	Length      int            `json:"length"`
	Terms       map[string]int `json:"terms,omitempty"`
}

// Local is this chunk's locator local part: path plus line range, which is
// exactly what expansion needs and what a person can act on by hand.
func (c indexedChunk) Local() string {
	return fmt.Sprintf("%s#L%d-L%d", c.Path, c.StartLine, c.EndLine)
}

type indexHeader struct {
	Format      int       `json:"format"`
	BuiltAt     time.Time `json:"built_at"`
	Watermark   string    `json:"watermark"`
	GitRevision string    `json:"git_revision,omitempty"`
	Root        string    `json:"root"`

	// SettingsDigest is the configuration this generation was built under.
	//
	// The watermark already contains it, but only a fresh corpus walk can
	// recover it from there. Recording it plainly lets the handshake decide,
	// without touching the corpus, whether the published generation describes
	// the boundary the current configuration asks for. A generation written
	// before this field existed reports the empty string, which matches no
	// configuration and is therefore rebuilt once.
	SettingsDigest string `json:"settings_digest,omitempty"`
}

// indexConfig identifies the retrieval configuration a generation was built
// under, per the spec's index obligations. It carries the settings digest as
// well as the scoring constants: the digest is what makes a corpus boundary
// change — an extension added, a directory excluded — visible as freshness
// evidence on every result, rather than as an unexplained change in what the
// source answers.
func indexConfig(h indexHeader) string {
	return fmt.Sprintf("jsonl/%d chunking=heading tokenizer=alnum-fold scoring=bm25-k%g-b%g settings=%s",
		h.Format, bm25K1, bm25B, orNone(h.SettingsDigest))
}

func orNone(s string) string {
	if s == "" {
		return "unrecorded"
	}
	return s
}

type indexTrailer struct {
	Docs     int                  `json:"docs"`
	Chunks   int                  `json:"chunks"`
	Failures int                  `json:"failures"`
	Coverage recall.IndexCoverage `json:"coverage"`
}

// record is one line of the generation file. The trailer is what makes
// truncation detectable: a build that died mid-write leaves a file that cannot
// load, so a partial index can never be mistaken for a complete boundary even
// if it were somehow published.
type record struct {
	Kind    string        `json:"kind"`
	Header  *indexHeader  `json:"header,omitempty"`
	Doc     *indexedDoc   `json:"doc,omitempty"`
	Chunk   *indexedChunk `json:"chunk,omitempty"`
	Failure *indexFailure `json:"failure,omitempty"`
	Trailer *indexTrailer `json:"trailer,omitempty"`
}

const (
	kindHeader  = "header"
	kindDoc     = "doc"
	kindChunk   = "chunk"
	kindFailure = "failure"
	kindTrailer = "trailer"
)

type posting struct {
	chunk int
	tf    int
}

// identifier is one name a document answers to, pre-tokenized.
type identifier struct {
	tokens []string
	alias  bool
}

// generation is a published index, immutable once loaded. Search reads it
// without a lock because it is never mutated: a rebuild publishes a new one and
// swaps the pointer.
type generation struct {
	id       string
	dir      string
	header   indexHeader
	docs     []indexedDoc
	chunks   []indexedChunk
	failures []indexFailure
	coverage recall.IndexCoverage

	docAt    map[string]int
	chunksOf map[string][]int
	idents   map[string][]identifier
	postings map[string][]posting
	avgLen   float64
}

func (g *generation) doc(path string) (indexedDoc, bool) {
	i, ok := g.docAt[path]
	if !ok {
		return indexedDoc{}, false
	}
	return g.docs[i], true
}

// confirmedAt reports when a complete source boundary last confirmed these
// records, and nothing when the boundary was not complete. A partial build saw
// only the files it managed to read, so claiming confirmation for all of them
// would overstate what the adapter knows.
func (g *generation) confirmedAt() *time.Time {
	if g.coverage != recall.IndexComplete {
		return nil
	}
	t := g.header.BuiltAt
	return &t
}

// buildIndex writes a new generation and publishes it atomically.
//
// Ordering is the whole point: everything is durable before the pointer moves,
// and the pointer moves in one rename. A failure at any step before that leaves
// the previous generation published and readable. The staging directory is
// removed on the way out of a failed build, and collected by the next build
// when the process died before it could run anything at all.
func buildIndex(ctx context.Context, root string, s Settings, indexDir string) (*generation, error) {
	corpus, err := scanCorpus(root, s)
	if err != nil {
		return nil, err
	}

	staging, err := os.MkdirTemp(indexDir, buildPrefix)
	if err != nil {
		return nil, fmt.Errorf("staging directory: %w", err)
	}
	staged := false
	defer func() {
		if !staged {
			_ = os.RemoveAll(staging)
		}
	}()

	digest, err := writeGeneration(ctx, staging, root, s, corpus)
	if err != nil {
		return nil, err
	}

	seq, err := nextSequence(indexDir)
	if err != nil {
		return nil, err
	}
	genDir := filepath.Join(indexDir, fmt.Sprintf("%s%06d-%s", genPrefix, seq, digest))
	if err := os.Rename(staging, genDir); err != nil {
		return nil, fmt.Errorf("stage generation: %w", err)
	}
	staged = true
	if err := syncDir(indexDir); err != nil {
		return nil, err
	}
	if err := publish(indexDir, filepath.Base(genDir)); err != nil {
		return nil, err
	}
	prune(indexDir, filepath.Base(genDir))

	return loadGeneration(genDir)
}

// writeGeneration streams the whole index into the staging directory and
// returns its content digest.
//
// The digest deliberately excludes the header: the same corpus indexed twice
// produces the same digest and a new sequence number, so an operator can see at
// a glance that a rebuild changed nothing.
func writeGeneration(ctx context.Context, staging, root string, s Settings, corpus scan) (string, error) {
	f, err := os.OpenFile(filepath.Join(staging, indexFile), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, genFilePerm)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	buf := bufio.NewWriter(f)
	hasher := sha256.New()
	head := json.NewEncoder(buf)
	body := json.NewEncoder(io.MultiWriter(buf, hasher))

	err = head.Encode(record{Kind: kindHeader, Header: &indexHeader{
		Format:         indexFormat,
		BuiltAt:        time.Now().UTC(),
		Watermark:      corpus.Watermark,
		GitRevision:    corpus.GitRevision,
		Root:           root,
		SettingsDigest: s.digest(),
	}})
	if err != nil {
		return "", err
	}

	var counts indexTrailer
	for _, ref := range corpus.Files {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("build interrupted: %w", err)
		}
		doc, chunks, failure := readDocument(root, ref, s)
		if failure != nil {
			counts.Failures++
			if err := body.Encode(record{Kind: kindFailure, Failure: failure}); err != nil {
				return "", err
			}
			continue
		}
		counts.Docs++
		if err := body.Encode(record{Kind: kindDoc, Doc: &doc}); err != nil {
			return "", err
		}
		for i := range chunks {
			counts.Chunks++
			if err := body.Encode(record{Kind: kindChunk, Chunk: &chunks[i]}); err != nil {
				return "", err
			}
		}
	}

	counts.Coverage = recall.IndexComplete
	if counts.Failures > 0 {
		// Some named records are absent from this generation. Absence here is
		// not deletion, and coverage is how the difference stays visible.
		counts.Coverage = recall.IndexPartial
	}
	if err := body.Encode(record{Kind: kindTrailer, Trailer: &counts}); err != nil {
		return "", err
	}

	if err := buf.Flush(); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := syncDir(staging); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil))[:digestLength], nil
}

// readDocument turns one file into a document and its chunks, or into the
// reason it could not be indexed.
//
// Every rejection here names one record. A build that aborted on the first
// unreadable file would let one corrupt note cost a corpus its whole index,
// while a build that silently skipped it would report complete coverage over an
// incomplete boundary.
func readDocument(root string, ref fileRef, s Settings) (indexedDoc, []indexedChunk, *indexFailure) {
	fail := func(reason string) (indexedDoc, []indexedChunk, *indexFailure) {
		return indexedDoc{}, nil, &indexFailure{Path: ref.Path, Reason: reason}
	}
	if ref.Size > s.MaxFileBytes {
		return fail(fmt.Sprintf("larger than max_file_bytes (%d > %d)", ref.Size, s.MaxFileBytes))
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ref.Path)))
	if err != nil {
		return fail("unreadable: " + errKind(err))
	}
	if !utf8.Valid(data) {
		return fail("not valid UTF-8")
	}
	if strings.IndexByte(string(data), 0) >= 0 {
		return fail("binary content")
	}

	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	parsed := parseChunks(lines)

	sum := sha256.Sum256(data)
	doc := indexedDoc{
		Path:        ref.Path,
		Title:       docTitle(parsed, strings.TrimSuffix(path.Base(ref.Path), path.Ext(ref.Path))),
		Project:     projectOf(ref.Path),
		Size:        ref.Size,
		ModTime:     ref.ModTime.UTC(),
		Revision:    hex.EncodeToString(sum[:])[:digestLength],
		Identifiers: pathIdentifiers(ref.Path),
		Aliases:     s.Aliases[ref.Path],
		ChunkCount:  len(parsed),
	}

	chunks := make([]indexedChunk, 0, len(parsed))
	for _, c := range parsed {
		terms, length := c.terms()
		chunks = append(chunks, indexedChunk{
			Path:        ref.Path,
			Ord:         c.Ord,
			StartLine:   c.StartLine,
			EndLine:     c.EndLine,
			Heading:     strings.TrimSpace(c.Heading),
			HeadingPath: c.HeadingPath,
			Excerpt:     c.excerpt(),
			Fingerprint: c.fingerprint(),
			Length:      length,
			Terms:       terms,
		})
	}
	return doc, chunks, nil
}

// errKind renders a filesystem failure without repeating the path, which the
// Failure record already carries.
func errKind(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

// projectOf keeps the corpus's own directory structure as routing metadata
// rather than flattening it into the text, per the document source class.
func projectOf(rel string) string {
	dir := path.Dir(rel)
	if dir == "." {
		return ""
	}
	first, _, _ := strings.Cut(dir, "/")
	return first
}

// pathIdentifiers are the names that may carry an exact_identifier signal.
//
// Every form here is path-shaped: it contains a directory separator or the file
// extension. A bare stem is excluded on purpose — "docs/adapter-protocol.md"
// would otherwise make the prose "the adapter protocol" an exact identifier
// match, and exact matches partition the whole result set above everything
// else. A one-word name earns that promotion only when someone declares it as
// an alias for this corpus.
func pathIdentifiers(rel string) []string {
	ext := path.Ext(rel)
	base := path.Base(rel)
	out := []string{rel, base}
	if strings.Contains(rel, "/") {
		out = append(out, strings.TrimSuffix(rel, ext))
	}

	seen := map[string]bool{}
	kept := make([]string, 0, len(out))
	for _, id := range out {
		if seen[id] || len(tokenize(id)) < 2 {
			continue
		}
		seen[id] = true
		kept = append(kept, id)
	}
	sort.Strings(kept)
	return kept
}

// openIndex loads the published generation, if there is one.
func openIndex(indexDir string) (*generation, error) {
	name, err := os.ReadFile(filepath.Join(indexDir, currentFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoGeneration
	}
	if err != nil {
		return nil, err
	}
	dir := strings.TrimSpace(string(name))
	if dir == "" || !strings.HasPrefix(dir, genPrefix) || strings.ContainsAny(dir, `/\`) {
		return nil, fmt.Errorf("docs: unusable generation pointer %q", dir)
	}
	return loadGeneration(filepath.Join(indexDir, dir))
}

// loadGeneration reads a generation and derives everything search needs.
//
// A file missing its trailer, or whose counts disagree with its records, is
// rejected rather than repaired. Half an index answering queries would be a
// source silently reporting a smaller corpus than it has.
func loadGeneration(dir string) (*generation, error) {
	f, err := os.Open(filepath.Join(dir, indexFile))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	gen := &generation{
		id:       filepath.Base(dir),
		dir:      dir,
		docAt:    map[string]int{},
		chunksOf: map[string][]int{},
		idents:   map[string][]identifier{},
		postings: map[string][]posting{},
	}

	dec := json.NewDecoder(bufio.NewReader(f))
	var trailer *indexTrailer
	for i := 0; ; i++ {
		var rec record
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("generation %s: record %d: %w", gen.id, i, err)
		}
		if trailer != nil {
			return nil, fmt.Errorf("generation %s: records after trailer", gen.id)
		}
		switch rec.Kind {
		case kindHeader:
			if i != 0 || rec.Header == nil {
				return nil, fmt.Errorf("generation %s: misplaced header", gen.id)
			}
			if rec.Header.Format != indexFormat {
				return nil, fmt.Errorf("generation %s: format %d, want %d", gen.id, rec.Header.Format, indexFormat)
			}
			gen.header = *rec.Header
		case kindDoc:
			if rec.Doc == nil {
				return nil, fmt.Errorf("generation %s: empty doc record", gen.id)
			}
			gen.docAt[rec.Doc.Path] = len(gen.docs)
			gen.docs = append(gen.docs, *rec.Doc)
		case kindChunk:
			if rec.Chunk == nil {
				return nil, fmt.Errorf("generation %s: empty chunk record", gen.id)
			}
			gen.chunks = append(gen.chunks, *rec.Chunk)
		case kindFailure:
			if rec.Failure == nil {
				return nil, fmt.Errorf("generation %s: empty failure record", gen.id)
			}
			gen.failures = append(gen.failures, *rec.Failure)
		case kindTrailer:
			if rec.Trailer == nil {
				return nil, fmt.Errorf("generation %s: empty trailer", gen.id)
			}
			trailer = rec.Trailer
		default:
			return nil, fmt.Errorf("generation %s: unknown record kind %q", gen.id, rec.Kind)
		}
	}

	if trailer == nil {
		return nil, fmt.Errorf("generation %s: truncated, no trailer", gen.id)
	}
	if trailer.Docs != len(gen.docs) || trailer.Chunks != len(gen.chunks) || trailer.Failures != len(gen.failures) {
		return nil, fmt.Errorf("generation %s: counts disagree with records", gen.id)
	}
	gen.coverage = trailer.Coverage
	gen.derive()
	return gen, nil
}

// derive builds the postings, identifier table, and average length. It runs
// once at load: the generation is immutable afterwards, which is what lets
// concurrent searches share it without a lock.
func (g *generation) derive() {
	total := 0
	for i, c := range g.chunks {
		g.chunksOf[c.Path] = append(g.chunksOf[c.Path], i)
		total += c.Length
		for _, term := range sortedKeys(c.Terms) {
			g.postings[term] = append(g.postings[term], posting{chunk: i, tf: c.Terms[term]})
		}
	}
	if len(g.chunks) > 0 {
		g.avgLen = float64(total) / float64(len(g.chunks))
	}
	for _, d := range g.docs {
		var ids []identifier
		for _, name := range d.Identifiers {
			ids = append(ids, identifier{tokens: tokenize(name)})
		}
		for _, name := range d.Aliases {
			ids = append(ids, identifier{tokens: tokenize(name), alias: true})
		}
		if len(ids) > 0 {
			g.idents[d.Path] = ids
		}
	}
}

// nextSequence continues the generation counter, so publication order is
// readable on disk.
func nextSequence(indexDir string) (int, error) {
	entries, err := os.ReadDir(indexDir)
	if err != nil {
		return 0, err
	}
	high := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, genPrefix) {
			continue
		}
		digits, _, _ := strings.Cut(strings.TrimPrefix(name, genPrefix), "-")
		if n, err := strconv.Atoi(digits); err == nil && n > high {
			high = n
		}
	}
	return high + 1, nil
}

// publish moves the pointer. This one rename is the publication: before it the
// new generation is invisible, after it the old one is unreferenced.
func publish(indexDir, name string) error {
	tmp := filepath.Join(indexDir, currentFile+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, genFilePerm)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(name + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(indexDir, currentFile)); err != nil {
		return err
	}
	return syncDir(indexDir)
}

// prune drops superseded generations and abandoned staging directories.
// Generations are not browsable history: a superseded one would be a second
// answer to the same question, and an expansion cache honoring it would
// resurface deleted records.
func prune(indexDir, keep string) {
	entries, err := os.ReadDir(indexDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if name == keep || name == currentFile {
			continue
		}
		if strings.HasPrefix(name, genPrefix) || strings.HasPrefix(name, buildPrefix) {
			_ = os.RemoveAll(filepath.Join(indexDir, name))
		}
	}
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", filepath.Base(dir), err)
	}
	return nil
}

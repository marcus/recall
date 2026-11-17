package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/marcus/recall/pkg/document"
)

// SemanticScan is a metadata-only corpus boundary. Scanning never embeds text.
type SemanticScan struct {
	Watermark string
	Files     []SemanticFile
}

// SemanticFile identifies a selected, corpus-relative file and its observed
// nanosecond modification time and size. Version is a change hint, not a hash.
type SemanticFile struct {
	Path    string
	Version string
}

// SemanticDocument carries live source lines and their raw-byte SHA256. HTML
// chunk text is extracted prose; Lines remain the original markup for citations.
// A robots noindex document is returned as SemanticDocument{} with a nil error.
type SemanticDocument struct {
	Title  string
	Lines  []string
	Hash   string
	Chunks []SemanticChunk
}

// SemanticChunk is bounded embedding text and a 1-based inclusive source span.
// HTML subdivisions share the containing extracted section's source span. Text
// never injects a document title or a heading from outside the cited span.
type SemanticChunk struct {
	Start, End  int
	Text        string
	Fingerprint string
}

// semanticChunkBytes is deliberately conservative: 1800 UTF-8 bytes leaves
// room in EmbeddingGemma's 2048-token context for its prefix and an ordinary
// title even when byte fallback is needed. Callers supplying unusually long
// titles must budget those separately. No tokenizer or inference is needed to
// scan or read a corpus. Keep this fixed as a persisted chunking contract.
const semanticChunkBytes = 1800

// ScanSemantic uses the lexical adapter's corpus selection and watermark. A
// profile name or semantic index location cannot change this corpus boundary.
func ScanSemantic(ctx context.Context, root string, settings map[string]any) (SemanticScan, error) {
	root, s, err := semanticSettings(root, settings)
	if err != nil {
		return SemanticScan{}, err
	}
	corpus, err := scanCorpus(ctx, root, s)
	if err != nil {
		return SemanticScan{}, err
	}
	out := SemanticScan{Watermark: corpus.Watermark, Files: make([]SemanticFile, 0, len(corpus.Files))}
	for _, f := range corpus.Files {
		out.Files = append(out.Files, SemanticFile{Path: f.Path, Version: fmt.Sprintf("%d:%d", f.ModTime.UnixNano(), f.Size)})
	}
	return out, nil
}

// ReadSemantic reads one selected path without following a symlink or escaping
// the corpus. The parser and HTML noindex rules are shared with readText; the
// confined open is needed because a semantic refresh may read after its scan.
func ReadSemantic(root, rel string, settings map[string]any) (SemanticDocument, error) {
	root, s, err := semanticSettings(root, settings)
	if err != nil {
		return SemanticDocument{}, err
	}
	if rel == "." || path.Clean(rel) != rel || !filepath.IsLocal(rel) || strings.ContainsAny(rel, "\\#") || !s.indexes(rel) {
		return SemanticDocument{}, errors.New("docs: invalid semantic corpus path")
	}
	if reason := s.selection().GlobReason(rel); reason != "" {
		return SemanticDocument{}, fmt.Errorf("docs: path excluded: %s", reason)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return SemanticDocument{}, err
	}
	defer func() { _ = r.Close() }()
	// Check every component, including parents replaced since ScanSemantic.
	// os.Root enforces confinement during the open even if these checks race.
	parts := strings.Split(rel, "/")
	before := make([]os.FileInfo, len(parts))
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		info, statErr := r.Lstat(prefix)
		if statErr != nil {
			return SemanticDocument{}, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || i == len(parts)-1 && !info.Mode().IsRegular() || i < len(parts)-1 && !info.IsDir() {
			return SemanticDocument{}, errors.New("docs: not a regular corpus file")
		}
		if i < len(parts)-1 {
			if reason := s.selection().DirectoryReason(root, prefix); reason != "" {
				return SemanticDocument{}, fmt.Errorf("docs: path excluded: %s", reason)
			}
		}
		before[i] = info
	}
	// Nonblocking prevents a concurrent replacement with a FIFO from hanging;
	// nofollow rejects a final-component symlink at the actual open boundary.
	f, err := r.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return SemanticDocument{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return SemanticDocument{}, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before[len(before)-1], info) {
		return SemanticDocument{}, errors.New("docs: corpus file changed during open")
	}
	for i := range parts {
		after, statErr := r.Lstat(strings.Join(parts[:i+1], "/"))
		if statErr != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before[i], after) {
			return SemanticDocument{}, errors.New("docs: corpus path changed during open")
		}
	}
	// Match readText's limits, then reuse its parser. Opening through readText
	// itself would discard the confined descriptor and reopen a racy pathname.
	limit := s.MaxFileBytes
	if document.IsHTML(rel) {
		limit = s.MaxRawBytes
	}
	if info.Size() > limit {
		return SemanticDocument{}, errors.New("docs: semantic file exceeds configured byte limit")
	}
	data, err := document.Read(f, limit)
	if err != nil {
		return SemanticDocument{}, err
	}
	text, err := parseText(data, rel, s)
	if errors.Is(err, document.ErrNoIndex) {
		return SemanticDocument{}, nil
	}
	if err != nil {
		return SemanticDocument{}, err
	}
	sum := sha256.Sum256(data)
	out := SemanticDocument{
		Title: text.title, Lines: strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"),
		Hash: hex.EncodeToString(sum[:]), Chunks: []SemanticChunk{},
	}
	for _, section := range text.chunks {
		lines := out.Lines[section.StartLine-1 : section.EndLine]
		if text.html != nil {
			lines = strings.Split(section.text(), "\n")
		}
		for _, chunk := range splitSemanticSection(lines, section.StartLine) {
			if text.html != nil {
				chunk.Start, chunk.End = section.StartLine, section.EndLine
			}
			out.Chunks = append(out.Chunks, chunk)
		}
	}
	return out, nil
}

func semanticSettings(root string, raw map[string]any) (string, Settings, error) {
	s, err := parseSettings(raw)
	if err != nil {
		return "", s, err
	}
	if s.Root != "" {
		root = s.Root
	}
	if root == "" {
		return "", s, errors.New("docs: no semantic corpus root")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", s, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", s, err
	}
	if !info.IsDir() {
		return "", s, errors.New("docs: semantic corpus root is not a directory")
	}
	return root, s, nil
}

// splitSemanticSection packs whole paragraphs until the byte cap. Oversized
// paragraphs split at lines, then whitespace, then UTF-8 boundaries. Long lines
// may therefore yield multiple chunks with the same source line. Nothing is
// truncated and headings never pull text across a parsed section boundary.
func splitSemanticSection(lines []string, first int) []SemanticChunk {
	var out []SemanticChunk
	emit := func(text string, start, end int) {
		if strings.TrimSpace(text) == "" {
			return
		}
		sum := sha256.Sum256([]byte(text))
		out = append(out, SemanticChunk{Start: start, End: end, Text: text, Fingerprint: hex.EncodeToString(sum[:])})
	}
	var pending string
	start, end := 0, 0
	flush := func() { emit(pending, start, end); pending = "" }
	for i := 0; i < len(lines); {
		if strings.TrimSpace(lines[i]) == "" {
			i++
			continue
		}
		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) != "" {
			j++
		}
		paragraph := strings.Join(lines[i:j], "\n")
		separator := ""
		if pending != "" {
			separator = strings.Repeat("\n", first+i-end)
		}
		if len(pending)+len(separator)+len(paragraph) <= semanticChunkBytes {
			if pending == "" {
				start = first + i
			}
			pending += separator + paragraph
			end = first + j - 1
		} else {
			flush()
			if len(paragraph) <= semanticChunkBytes {
				pending, start, end = paragraph, first+i, first+j-1
			} else {
				for k := i; k < j; k++ {
					line := lines[k]
					for len(line) > semanticChunkBytes {
						flush()
						cut := semanticCut(line)
						emit(line[:cut], first+k, first+k)
						line = line[cut:]
					}
					if pending != "" && len(pending)+1+len(line) > semanticChunkBytes {
						flush()
					}
					if pending == "" {
						pending, start = line, first+k
					} else {
						pending += "\n" + line
					}
					end = first + k
				}
			}
		}
		i = j
	}
	flush()
	return out
}

func semanticCut(text string) int {
	cut := semanticChunkBytes
	for !utf8.RuneStart(text[cut]) {
		cut--
	}
	// Prefer a word boundary in the latter half, avoiding tiny chunks when a
	// paragraph contains a long identifier or a language without spaces.
	lastSpace := 0
	for i, r := range text[:cut] {
		if i >= cut/2 && unicode.IsSpace(r) {
			lastSpace = i + utf8.RuneLen(r)
		}
	}
	if lastSpace > 0 {
		return lastSpace
	}
	return cut
}

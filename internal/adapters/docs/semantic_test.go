package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

func semanticFixture(t *testing.T, root, rel, text string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScanSemanticMetadataAndSelection(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"note.md", "note.html", "keep.html", "ignored.md", ".hidden/a.md", "node_modules/a.md", "nested/.git/HEAD", "nested/a.md"} {
		// Invalid UTF-8 is intentionally admitted by the scan, not parsed.
		semanticFixture(t, root, p, "\xff")
	}
	s := map[string]any{
		"extensions": []string{".md", ".html"}, "exclude_globs": []string{"ignored.md"},
		"sibling_preference": []string{".md", ".html"},
	}
	first, err := ScanSemantic(context.Background(), root, s)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range first.Files {
		paths = append(paths, f.Path)
		if f.Version == "" {
			t.Fatal("missing file version")
		}
	}
	if !reflect.DeepEqual(paths, []string{"keep.html", "note.md"}) {
		t.Fatalf("selected %v", paths)
	}
	// A query-only profile setting does not invalidate the corpus.
	s["examples_quote_queries"] = true
	second, err := ScanSemantic(context.Background(), root, s)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("unstable profile scan: %#v, %v", second, err)
	}
	if _, err := ReadSemantic(root, "note.md", s); err == nil {
		t.Fatal("reader accepted invalid text that metadata scan correctly admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ScanSemantic(ctx, root, s); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan: %v", err)
	}
}

func TestScanSemanticSubsecondVersion(t *testing.T) {
	root := t.TempDir()
	semanticFixture(t, root, "note.md", "before")
	file := filepath.Join(root, "note.md")
	stamp := time.Unix(1800000000, 123456000)
	if err := os.Chtimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	before, err := ScanSemantic(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	semanticFixture(t, root, "note.md", "after!")
	stamp = stamp.Add(time.Millisecond)
	if err := os.Chtimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	after, err := ScanSemantic(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before.Files[0].Version == after.Files[0].Version || before.Watermark == after.Watermark {
		t.Fatal("same-size subsecond edit did not change version and watermark")
	}
}

func TestReadSemanticMarkdownChunks(t *testing.T) {
	root := t.TempDir()
	first := strings.Repeat("first word ", 90)
	second := strings.Repeat("second word ", 90)
	raw := "# Title\r\n\r\n" + first + "\r\n\r\n" + second + "\r\n\r\n## Next\r\n\r\nEvidence\r\n```sh\r\n# not a heading\r\n```\r\n"
	semanticFixture(t, root, "note.md", raw)
	doc, err := ReadSemantic(root, "note.md", nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(raw))
	if doc.Title != "Title" || doc.Hash != hex.EncodeToString(sum[:]) || doc.Lines[0] != "# Title" {
		t.Fatalf("document identity: %#v", doc)
	}
	if len(doc.Chunks) != 3 {
		t.Fatalf("want paragraph/heading split, got %#v", doc.Chunks)
	}
	for _, c := range doc.Chunks {
		if len(c.Text) > semanticChunkBytes || c.Start < 1 || c.End > len(doc.Lines) || c.Start > c.End {
			t.Fatalf("invalid chunk: %#v", c)
		}
		if want := strings.Join(doc.Lines[c.Start-1:c.End], "\n"); c.Text != want {
			t.Fatalf("text left its source span: %q != %q", c.Text, want)
		}
		sum := sha256.Sum256([]byte(c.Text))
		if c.Fingerprint != hex.EncodeToString(sum[:]) {
			t.Fatal("fingerprint is not a complete text identity")
		}
	}
	if strings.Contains(doc.Chunks[1].Text, "# Title") || !strings.Contains(doc.Chunks[2].Text, "# not a heading") {
		t.Fatal("headings injected outside span or code fence changed section parsing")
	}
	again, err := ReadSemantic(root, "note.md", nil)
	if err != nil || !reflect.DeepEqual(doc, again) {
		t.Fatal("unstable document read")
	}
}

func TestReadSemanticLongLineWithoutLoss(t *testing.T) {
	for _, text := range []string{strings.Repeat("世界", 1100), strings.Repeat("long-word ", 800), strings.Repeat("x", 5000)} {
		t.Run(text[:1], func(t *testing.T) {
			root := t.TempDir()
			semanticFixture(t, root, "note.md", text)
			doc, err := ReadSemantic(root, "note.md", nil)
			if err != nil {
				t.Fatal(err)
			}
			var joined strings.Builder
			for _, c := range doc.Chunks {
				if len(c.Text) > semanticChunkBytes || !utf8.ValidString(c.Text) || c.Start != 1 || c.End != 1 {
					t.Fatalf("invalid long-line chunk: %#v", c)
				}
				joined.WriteString(c.Text)
			}
			if joined.String() != text {
				t.Fatal("long-line splitting lost or duplicated text")
			}
		})
	}
}

func TestReadSemanticHTMLAndNoindex(t *testing.T) {
	root := t.TempDir()
	raw := "<title>Page</title>\n<script>secret script</script>\n<h1>Heading</h1>\n<p>Visible evidence</p>\n<div hidden>secret hidden</div>\n<h2>Next</h2>\n<p>More evidence</p>"
	semanticFixture(t, root, "page.html", raw)
	s := map[string]any{"extensions": []string{".html"}}
	doc, err := ReadSemantic(root, "page.html", s)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(raw))
	if doc.Hash != hex.EncodeToString(sum[:]) || doc.Title != "Page" || len(doc.Lines) != 7 || len(doc.Chunks) != 2 {
		t.Fatalf("HTML document: %#v", doc)
	}
	if doc.Chunks[0].Start != 3 || doc.Chunks[0].End != 5 || doc.Chunks[0].Text != "# Heading\nVisible evidence" || doc.Chunks[1].Start != 6 {
		t.Fatalf("wrong extracted text/span: %#v", doc.Chunks)
	}
	semanticFixture(t, root, "page.html", "<meta name=robots content='noindex'><p>private</p>")
	noindex, err := ReadSemantic(root, "page.html", s)
	if err != nil || !reflect.DeepEqual(noindex, SemanticDocument{}) {
		t.Fatalf("noindex contract: %#v, %v", noindex, err)
	}
	// Extraction limits are charged to visible text, separately from raw bytes.
	semanticFixture(t, root, "page.html", "<p>"+strings.Repeat("x", 100)+"</p>")
	s["max_file_bytes"] = 20
	if _, err := ReadSemantic(root, "page.html", s); err == nil {
		t.Fatal("extracted text limit was ignored")
	}
	delete(s, "max_file_bytes")
	semanticFixture(t, root, "page.html", "<h1>Large</h1>\n<p>"+strings.Repeat("word ", 1200)+"</p>")
	large, err := ReadSemantic(root, "page.html", s)
	if err != nil || len(large.Chunks) < 2 {
		t.Fatalf("large HTML section was not split: %#v, %v", large, err)
	}
	for _, c := range large.Chunks {
		if c.Start != 1 || c.End != 2 || len(c.Text) > semanticChunkBytes || strings.Contains(c.Text, "<p>") {
			t.Fatalf("HTML subdivision lost extracted-text source span: %#v", c)
		}
	}
}

func TestReadSemanticConcurrentPathReplacement(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	semanticFixture(t, root, "real/note.md", "allowed")
	semanticFixture(t, outside, "note.md", "outside corpus")
	stop, done := make(chan struct{}), make(chan error, 1)
	go func() {
		current, parked := filepath.Join(root, "real"), filepath.Join(root, "parked")
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			for _, op := range []func() error{
				func() error { return os.Rename(current, parked) },
				func() error { return os.Symlink(outside, current) },
				func() error { return os.Remove(current) },
				func() error { return os.Rename(parked, current) },
			} {
				if err := op(); err != nil {
					done <- err
					return
				}
			}
		}
	}()
	defer func() {
		close(stop)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for range 100 {
		doc, err := ReadSemantic(root, "real/note.md", nil)
		// A path being replaced may be unavailable, but any successful read
		// must still be the regular file inside the explicitly configured root.
		if err == nil && (len(doc.Chunks) != 1 || doc.Chunks[0].Text != "allowed") {
			t.Fatalf("read through a concurrent escaping symlink: %#v", doc)
		}
	}
}

func TestReadSemanticRejectsUnsafePaths(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	semanticFixture(t, root, "real/note.md", "allowed")
	semanticFixture(t, outside, "note.md", "outside")
	for name, target := range map[string]string{"escape": outside, "internal": "real", "linked.md": "real/note.md"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../note.md", filepath.Join(outside, "note.md"), "real/../real/note.md", "escape/note.md", "internal/note.md", "linked.md", "pipe.md", "real", "real\\note.md", "real/note.md#L1-L1"} {
		if _, err := ReadSemantic(root, rel, nil); err == nil {
			t.Errorf("accepted unsafe path %q", rel)
		}
	}
	if _, err := ReadSemantic(root, "real/note.md", map[string]any{"exclude_dirs": []string{"real"}}); err == nil {
		t.Fatal("excluded directory accepted")
	}
	if _, err := ReadSemantic(root, "real/note.md", map[string]any{"exclude_globs": []string{"**/note.md"}}); err == nil {
		t.Fatal("excluded file accepted")
	}
}

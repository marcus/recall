package docs

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/marcus/recall/pkg/document"
)

type documentText struct {
	data   []byte
	title  string
	chunks []parsedChunk
	html   *document.HTML
}

// readText is the single live reader for indexing, query excerpts and expansion.
// Markdown preserves its parser and raw-text cap; HTML charges extracted text.
func readText(root, rel string, s Settings) (documentText, error) {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return documentText{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return documentText{}, err
	}
	if !info.Mode().IsRegular() {
		return documentText{}, fmt.Errorf("not a regular file")
	}
	rawLimit := s.MaxRawBytes
	if rawLimit <= 0 {
		rawLimit = document.DefaultMaxRawBytes
	}
	maxText := s.MaxFileBytes
	if maxText <= 0 {
		maxText = defaultMaxFileBytes
	}
	if !document.IsHTML(rel) {
		rawLimit = maxText
	}
	if info.Size() > rawLimit {
		key := "max_raw_bytes"
		if !document.IsHTML(rel) {
			key = "max_file_bytes"
		}
		return documentText{}, fmt.Errorf("larger than %s (%d > %d)", key, info.Size(), rawLimit)
	}
	data, err := document.Read(f, rawLimit)
	if err != nil {
		return documentText{}, err
	}
	return parseText(data, rel, s)
}

func parseText(data []byte, rel string, s Settings) (documentText, error) {
	out := documentText{data: data}
	if document.IsHTML(rel) {
		parsed, err := document.ExtractHTML(data, rel, s.MaxRawBytes, s.MaxFileBytes)
		if err != nil {
			return documentText{}, err
		}
		out.html = &parsed
		out.title = parsed.Title
		for i, c := range parsed.Chunks {
			out.chunks = append(out.chunks, parsedChunk{Ord: i, Heading: c.Heading, HeadingPath: c.HeadingPath, Anchor: c.Anchor, StartLine: c.StartLine, EndLine: c.EndLine, Body: c.Body})
		}
	} else {
		out.chunks = parseChunks(strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"))
		out.title = docTitle(out.chunks, strings.TrimSuffix(path.Base(rel), path.Ext(rel)))
	}
	return out, nil
}

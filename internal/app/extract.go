package app

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/marcus/recall/pkg/document"
)

// ExtractedHTML is a disposable text projection, with source anchors and lines.
// Headings includes a heading-less preamble when the document has one.
type ExtractedHTML struct {
	Title             string             `json:"title"`
	Headings          []ExtractedSection `json:"headings"`
	Text              string             `json:"text"`
	NoIndex           bool               `json:"noindex"`
	ExtractionVersion int                `json:"extractionVersion"`
}

type ExtractedSection struct {
	Heading   string `json:"heading"`
	Level     int    `json:"level"`
	Anchor    string `json:"anchor,omitempty"`
	Text      string `json:"text"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
}

// ExtractHTMLFile uses the same bounded parser as retrieval, without loading a
// profile, reaching a provider, or writing an index. A noindex file returns an
// empty, explicitly excluded projection rather than exposing its metadata.
func ExtractHTMLFile(filename string) (ExtractedHTML, error) {
	out := ExtractedHTML{Headings: []ExtractedSection{}, ExtractionVersion: document.HTMLExtractionVersion}
	if !document.IsHTML(filename) {
		return out, fmt.Errorf("extract requires an HTML file")
	}
	before, err := os.Stat(filename)
	if err != nil {
		return out, err
	}
	if !before.Mode().IsRegular() {
		return out, fmt.Errorf("extract requires a regular file")
	}
	f, err := os.Open(filename)
	if err != nil {
		return out, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return out, err
	}
	if !info.Mode().IsRegular() {
		return out, fmt.Errorf("extract requires a regular file")
	}
	data, err := document.Read(f, 0)
	if err != nil {
		return out, err
	}
	doc, err := document.ExtractHTML(data, filename, 0, 0)
	if errors.Is(err, document.ErrNoIndex) {
		out.NoIndex = true
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Title = doc.Title
	var text []string
	for _, c := range doc.Chunks {
		out.Headings = append(out.Headings, ExtractedSection{
			Heading: strings.TrimSpace(strings.TrimLeft(c.Heading, "#")),
			Level:   c.Level, Anchor: c.Anchor, Text: strings.Join(c.Body, "\n"),
			StartLine: c.StartLine, EndLine: c.EndLine,
		})
		text = append(text, c.Text())
	}
	out.Text = strings.Join(text, "\n\n")
	return out, nil
}

// Package document provides shared document extraction and corpus-selection
// rules for lexical and derived semantic views. It performs no index writes.
package document

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const DefaultMaxRawBytes int64 = 16 << 20

// HTMLExtractionVersion identifies persisted text produced by ExtractHTML.
// Consumers must invalidate cached extraction when this version changes.
const HTMLExtractionVersion = 2

var ErrNoIndex = errors.New("robots noindex")

// Chunk is one heading-bounded section. Source lines are 1-based, inclusive;
// adjacent headings on the same source line necessarily have overlapping ranges.
type Chunk struct {
	Heading            string
	HeadingPath        []string
	Level              int
	Anchor             string
	StartLine, EndLine int
	Body               []string
}

func (c Chunk) Text() string {
	body := strings.Join(c.Body, "\n")
	if c.Heading == "" {
		return body
	}
	return strings.TrimRight(c.Heading+"\n"+body, "\n")
}

type HTML struct {
	Title       string
	Chunks      []Chunk
	SourceLines int
}

// Markdown renders the same text used by the lexical view, with a document title
// for Markdown-aware consumers. The original source remains the citation target.
func (d HTML) Markdown() string {
	parts := []string{"# " + d.Title}
	for _, c := range d.Chunks {
		if text := c.Text(); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n") + "\n"
}

func IsHTML(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".html" || ext == ".htm"
}

// Read bounds allocation even if a regular file grows after stat. Callers must
// reject non-regular files before opening them. Zero selects the default cap.
func Read(r io.Reader, maxRawBytes int64) ([]byte, error) {
	if maxRawBytes <= 0 {
		maxRawBytes = DefaultMaxRawBytes
	}
	data, err := io.ReadAll(io.LimitReader(r, maxRawBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxRawBytes {
		return nil, fmt.Errorf("larger than max_raw_bytes (%d)", maxRawBytes)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("not valid UTF-8")
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("binary content")
	}
	return data, nil
}

// ExtractHTML parses bounded UTF-8 HTML with x/net/html. A tokenizer first records
// byte offsets on element start tags using a private attribute; the tree parser
// then supplies HTML's implied closures and malformed-markup recovery. Markers
// never enter extracted text. Tracking source offsets before parsing avoids
// confusing tree order (e.g. foster-parented table text) with source positions.
// maxFileBytes bounds extracted text, not embedded images, CSS or other markup.
func ExtractHTML(data []byte, filename string, maxRawBytes, maxFileBytes int64) (HTML, error) {
	if maxRawBytes <= 0 {
		maxRawBytes = DefaultMaxRawBytes
	}
	if int64(len(data)) > maxRawBytes {
		return HTML{}, fmt.Errorf("larger than max_raw_bytes (%d)", maxRawBytes)
	}
	if !utf8.Valid(data) {
		return HTML{}, errors.New("not valid UTF-8")
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return HTML{}, errors.New("binary content")
	}
	sum := sha256.Sum256(data)
	marker := fmt.Sprintf("data-recall-source-%x", sum[:8])
	lowerData := bytes.ToLower(data)
	for bytes.Contains(lowerData, []byte(marker)) {
		marker += "x"
	}
	var marked bytes.Buffer
	z := html.NewTokenizer(bytes.NewReader(data))
	line, offset := 1, 0
	lines := map[string]int{}
	for {
		tt := z.Next()
		raw := z.Raw()
		if tt == html.ErrorToken {
			if !errors.Is(z.Err(), io.EOF) {
				return HTML{}, z.Err()
			}
			break
		}
		if tt == html.StartTagToken || tt == html.SelfClosingTagToken {
			id := strconv.Itoa(offset)
			lines[id] = line
			end := len(raw) - 1
			if end > 0 && raw[end-1] == '/' {
				end--
			}
			marked.Write(raw[:end])
			fmt.Fprintf(&marked, ` %s="%s"`, marker, id)
			marked.Write(raw[end:])
		} else {
			marked.Write(raw)
		}
		line += bytes.Count(raw, []byte{'\n'})
		offset += len(raw)
	}
	root, err := html.Parse(&marked)
	if err != nil {
		return HTML{}, err
	}
	out := HTML{SourceLines: bytes.Count(data, []byte{'\n'}) + 1}
	var title, firstH1 string
	// robots applies to the document, including when the head has no visible text.
	var metadata func(*html.Node)
	metadata = func(n *html.Node) {
		if excluded(n) {
			return
		}
		if n.Type == html.ElementNode {
			if n.Data == "title" && title == "" {
				title = plain(n)
			}
			if n.Data == "meta" && strings.EqualFold(attr(n, "name"), "robots") {
				for _, token := range strings.FieldsFunc(strings.ToLower(attr(n, "content")), func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
					if token == "noindex" {
						err = ErrNoIndex
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			metadata(c)
		}
	}
	metadata(root)
	if err != nil {
		return HTML{}, err
	}
	var stack []struct {
		level int
		text  string
	}
	current := Chunk{StartLine: 1}
	var buf strings.Builder
	flush := func() {
		text := collapse(buf.String())
		buf.Reset()
		if text != "" {
			current.Body = append(current.Body, text)
		}
	}
	blank := func() {
		flush()
		if len(current.Body) > 0 && current.Body[len(current.Body)-1] != "" {
			current.Body = append(current.Body, "")
		}
	}
	closeChunk := func(end int) {
		flush()
		for len(current.Body) > 0 && current.Body[len(current.Body)-1] == "" {
			current.Body = current.Body[:len(current.Body)-1]
		}
		current.EndLine = max(current.StartLine, end)
		if current.Heading != "" || len(current.Body) > 0 {
			out.Chunks = append(out.Chunks, current)
		}
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			buf.WriteString(n.Data)
			return
		}
		if n.Type != html.ElementNode && n.Type != html.DocumentNode {
			return
		}
		if excluded(n) || n.Data == "head" {
			return
		}
		level := headingLevel(n.Data)
		if level > 0 {
			text := plain(n)
			start := lines[attr(n, marker)]
			if start < 1 {
				start = 1
			}
			closeChunk(start - 1)
			for len(stack) > 0 && stack[len(stack)-1].level >= level {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, struct {
				level int
				text  string
			}{level, text})
			hp := make([]string, len(stack))
			for i, h := range stack {
				hp[i] = h.text
			}
			current = Chunk{Heading: strings.Repeat("#", level) + " " + text, HeadingPath: hp, Level: level, Anchor: attr(n, "id"), StartLine: start}
			if level == 1 && firstH1 == "" {
				firstH1 = text
			}
			return
		}
		switch n.Data {
		case "img":
			buf.WriteString(" " + attr(n, "alt") + " ")
			return
		case "br":
			flush()
			return
		case "li":
			flush()
			before := len(current.Body)
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			flush()
			for i := before; i < len(current.Body); i++ {
				if current.Body[i] != "" {
					current.Body[i] = "- " + current.Body[i]
					break
				}
			}
			return
		case "tr":
			if containsHeading(n) {
				flush()
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
				flush()
				return
			}
			flush()
			var cells []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") && !excluded(c) {
					cells = append(cells, plain(c))
				}
			}
			if len(cells) > 0 {
				current.Body = append(current.Body, strings.Join(cells, " | "))
			}
			return
		case "td", "th":
			if strings.TrimSpace(buf.String()) != "" {
				buf.WriteString(" | ")
			}
		default:
			if block(n.Data) {
				blank()
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		switch n.Data {
		case "li", "tr":
			flush()
		case "td", "th": // the row, rather than each cell, owns the line
		default:
			if block(n.Data) {
				blank()
			}
		}
	}
	walk(root)
	closeChunk(out.SourceLines)
	out.Title = title
	if out.Title == "" {
		out.Title = firstH1
	}
	if out.Title == "" {
		out.Title = path.Base(filename)
	}
	// Charge exactly the derived Markdown representation, shared by both views.
	if maxFileBytes > 0 && int64(len(out.Markdown())) > maxFileBytes {
		return HTML{}, fmt.Errorf("extracted text larger than max_file_bytes (%d > %d)", len(out.Markdown()), maxFileBytes)
	}
	return out, nil
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func excluded(n *html.Node) bool {
	switch n.Data {
	case "script", "style", "svg", "math", "template", "noscript", "iframe":
		return true
	}
	for _, a := range n.Attr {
		if a.Key == "data-noindex" || a.Key == "hidden" || a.Key == "aria-hidden" && strings.EqualFold(strings.TrimSpace(a.Val), "true") {
			return true
		}
		if a.Key == "style" {
			for _, rule := range strings.Split(strings.ToLower(a.Val), ";") {
				key, value, ok := strings.Cut(rule, ":")
				if !ok {
					continue
				}
				value = strings.TrimSpace(strings.Split(value, "!")[0])
				key = strings.TrimSpace(key)
				if key == "display" && value == "none" || key == "visibility" && (value == "hidden" || value == "collapse") {
					return true
				}
			}
		}
	}
	return false
}
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }
func plain(n *html.Node) string {
	var b strings.Builder
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if excluded(n) {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && n.Data == "img" {
			b.WriteString(" " + attr(n, "alt") + " ")
		}
		if n.Type == html.ElementNode && (block(n.Data) || n.Data == "br") {
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
		if n.Type == html.ElementNode && block(n.Data) {
			b.WriteByte(' ')
		}
	}
	visit(n)
	return collapse(b.String())
}
func headingLevel(tag string) int {
	if len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6' {
		return int(tag[1] - '0')
	}
	return 0
}
func block(tag string) bool {
	switch tag {
	case "p", "div", "section", "article", "main", "header", "footer", "nav", "aside", "blockquote", "pre", "ul", "ol", "dl", "dt", "dd", "table", "tr", "figure", "figcaption", "details", "summary", "hr":
		return true
	}
	return false
}

func containsHeading(n *html.Node) bool {
	if excluded(n) {
		return false
	}
	if headingLevel(n.Data) > 0 {
		return true
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if containsHeading(c) {
			return true
		}
	}
	return false
}

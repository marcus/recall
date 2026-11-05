package document_test

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/marcus/recall/pkg/document"
)

func TestHTMLStructuredTextAndSourceRanges(t *testing.T) {
	data, err := os.ReadFile("testdata/report.html")
	if err != nil {
		t.Fatal(err)
	}
	got, err := document.ExtractHTML(data, "report.html", 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Field & Garden Report" {
		t.Fatalf("title: %q", got.Title)
	}
	if len(got.Chunks) != 4 {
		t.Fatalf("chunks: %#v", got.Chunks)
	}
	wantPaths := [][]string{{"Garden"}, {"Garden", "Harvest"}, {"Garden", "Harvest", "Storage"}, {"Garden", "Winter"}}
	starts := []int{9, 21, 28, 30}
	ends := []int{20, 27, 29, 34}
	anchors := []string{"garden", "harvest", "storage", ""}
	for i, c := range got.Chunks {
		if !reflect.DeepEqual(c.HeadingPath, wantPaths[i]) || c.StartLine != starts[i] || c.EndLine != ends[i] || c.Anchor != anchors[i] {
			t.Errorf("chunk %d: %#v", i, c)
		}
	}
	text := got.Markdown()
	for _, want := range []string{"Visible orchard & apples.", "golden sunflower", "- Pears", "- Plums", "Crop | Yield", "Carrots | Abundant", "Autumn baskets", "Cellar instructions", "Keep turnips cool.", "### Storage"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	for _, bad := range []string{"poison", "<p>", "data:image", "hiddenheading"} {
		if strings.Contains(text, bad) {
			t.Errorf("leaked %q: %s", bad, text)
		}
	}
}

func TestHTMLTitleFallbackAndRecovery(t *testing.T) {
	for _, tc := range []struct{ html, title string }{
		{"<h1>First <b>heading</b></h1><h1>Second</h1>", "First heading"},
		{"<p>Just prose", "report.htm"},
		{"<title>Explicit</title><h1>First</h1>", "Explicit"},
	} {
		got, err := document.ExtractHTML([]byte(tc.html), "report.htm", 0, 10000)
		if err != nil || got.Title != tc.title {
			t.Errorf("got %#v, %v", got, err)
		}
	}
	// Implied li/td closures follow HTML parsing, and heading-like text in a
	// raw-text element is never mistaken for a section.
	got, err := document.ExtractHTML([]byte("<h1>A</h1><script><h2>bad</h2></script><ul><li>One<li>Two</ul><table><tr><td>Left<td>Right</table>"), "x.html", 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Chunks) != 1 || !strings.Contains(got.Markdown(), "- One\n- Two") || !strings.Contains(got.Markdown(), "Left | Right") {
		t.Fatal(got.Markdown())
	}
}

func TestHTMLLimitsAndNoIndex(t *testing.T) {
	raw := []byte("<h1>Photos</h1><img alt='Visible photo' src='data:image/png;base64," + strings.Repeat("A", 600000) + "'>")
	got, err := document.ExtractHTML(raw, "photos.html", 0, 1024)
	if err != nil || !strings.Contains(got.Markdown(), "Visible photo") {
		t.Fatalf("embedded image: %v", err)
	}
	if _, err := document.ExtractHTML(raw, "x.html", 100, 1024); err == nil {
		t.Fatal("raw limit not enforced")
	}
	if _, err := document.Read(strings.NewReader(strings.Repeat("x", 101)), 100); err == nil {
		t.Fatal("read limit not enforced")
	}
	if _, err := document.ExtractHTML([]byte("<p>"+strings.Repeat("text ", 100)), "x.html", 0, 50); err == nil {
		t.Fatal("text limit not enforced")
	}
	for _, robots := range []string{"NOINDEX,follow", "follow noindex", "noindex"} {
		_, err := document.ExtractHTML([]byte("<head><meta name='ROBOTS' content='"+robots+"'></head><h1>Private</h1>"), "x.html", 0, 10000)
		if !errors.Is(err, document.ErrNoIndex) {
			t.Fatalf("robots %q: %v", robots, err)
		}
	}
	for _, raw := range [][]byte{{0xff}, {0}} {
		if _, err := document.ExtractHTML(raw, "x.html", 0, 1000); err == nil {
			t.Fatal("accepted invalid text")
		}
	}
}

func TestHTMLBlockDetailsAndSourceOffsets(t *testing.T) {
	raw := "<title>Résumé</title>\r\n<h1\r\n id='first'>First</h1>\r\n<ul><li><p>First item</p><ul><li>Nested item</li></ul></li></ul>\r\n<table>\r\n<tr> <td><p>Alpha</p><p>Beta</p></td> <td>Gamma</td> </tr>\r\n<tr><td><h2 id='cell'>Cell heading</h2>Cell evidence</td></tr>\r\n</table>"
	got, err := document.ExtractHTML([]byte(raw), "x.html", 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Chunks) != 2 || got.Chunks[0].StartLine != 2 || got.Chunks[0].EndLine != 6 || got.Chunks[1].StartLine != 7 || got.Chunks[1].Anchor != "cell" {
		t.Fatalf("ranges: %#v", got.Chunks)
	}
	for _, want := range []string{"- First item", "- Nested item", "Alpha Beta | Gamma", "## Cell heading", "Cell evidence"} {
		if !strings.Contains(got.Markdown(), want) {
			t.Errorf("missing %q: %s", want, got.Markdown())
		}
	}
}

func TestHTMLDataNoIndexSubtrees(t *testing.T) {
	raw, err := os.ReadFile("testdata/noindex.html")
	if err != nil {
		t.Fatal(err)
	}
	got, err := document.ExtractHTML(raw, "proposal.html", 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	want := document.HTML{Title: "Proposal", SourceLines: 19, Chunks: []document.Chunk{
		{Heading: "# Design", HeadingPath: []string{"Design"}, Level: 1, Anchor: "proposal", StartLine: 3, EndLine: 11, Body: []string{"Outside orchard."}},
		{Heading: "## Decision ready", HeadingPath: []string{"Design", "Decision ready"}, Level: 2, Anchor: "decision", StartLine: 12, EndLine: 19, Body: []string{"Outside evidence.", "", "- Keep apples", "", "Crop | Yield", "Pears | Plenty", "", "Visible blossom"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extraction:\n%#v\nchunks: %#v\ntext: %s", got, got.Chunks, got.Markdown())
	}
}

func TestHTMLDataNoIndexPresenceAndMetadata(t *testing.T) {
	for _, marker := range []string{"data-noindex", `data-noindex=""`, `data-noindex="false"`, `DATA-NOINDEX="true"`, `data-noindex="anything"`} {
		t.Run(marker, func(t *testing.T) {
			for _, raw := range []string{
				"<head " + marker + "><title>mocktitle</title><meta name=robots content=noindex></head><body><h1>Outside</h1></body>",
				"<head><title " + marker + ">mocktitle</title><meta " + marker + " name=robots content=noindex></head><h1>Outside</h1>",
				"<div " + marker + "><title>mocktitle</title><meta name=robots content=noindex><h1>mockheading</h1></div><h1>Outside</h1>",
				"<h1 " + marker + ">mockheading</h1><h1>Outside</h1>",
			} {
				got, err := document.ExtractHTML([]byte(raw), "x.html", 0, 10000)
				if err != nil || got.Title != "Outside" || strings.Contains(got.Markdown(), "mock") || len(got.Chunks) != 1 {
					t.Fatalf("%s: %#v, %v", raw, got, err)
				}
			}
			got, err := document.ExtractHTML([]byte("<h1 "+marker+">mockheading</h1><p>Outside</p>"), "fallback.html", 0, 10000)
			if err != nil || got.Title != "fallback.html" || strings.Contains(got.Markdown(), "mock") || got.Chunks[0].Heading != "" {
				t.Fatalf("filename fallback: %#v, %v", got, err)
			}
			// A marker is local; an unmarked robots directive still excludes the file.
			_, err = document.ExtractHTML([]byte("<div "+marker+">mock</div><meta name=robots content=noindex><p>Outside</p>"), "x.html", 0, 10000)
			if !errors.Is(err, document.ErrNoIndex) {
				t.Fatalf("unmarked robots: %v", err)
			}
		})
	}
}

func TestHTMLDataNoIndexUsesParsedTree(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		// The heading implicitly closes p, so it is outside the marked subtree.
		{`<p data-noindex>mockbody<h1>Outside</h1><p>Evidence</p>`, "# Outside\n\n# Outside\nEvidence\n"},
		// Invalid table text is foster-parented outside the marked table.
		{`<h1>Outside</h1><table data-noindex>Outside text<tr><td>mockcell</td></tr></table>`, "# Outside\n\n# Outside\nOutside text\n"},
		{`<h1>Outside</h1><ul><li data-noindex>mockitem<li>Visible</ul>`, "# Outside\n\n# Outside\n- Visible\n"},
	} {
		got, err := document.ExtractHTML([]byte(tc.raw), "x.html", 0, 10000)
		if err != nil || got.Markdown() != tc.want {
			t.Fatalf("%s: %q, %v", tc.raw, got.Markdown(), err)
		}
	}
}

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

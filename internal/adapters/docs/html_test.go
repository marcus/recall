package docs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/adapters/docs"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

func htmlSettings() map[string]any {
	return map[string]any{"extensions": []any{".md", ".html", ".htm"}}
}

func TestHTMLIndexQueryAndExpand(t *testing.T) {
	data, err := os.ReadFile("../../../pkg/document/testdata/report.html")
	if err != nil {
		t.Fatal(err)
	}
	root := writtenCorpus(t, map[string]string{"report.html": string(data), "ordinary.md": "# Ordinary\nStable Markdown."})
	a, _ := newAdapter(t, root, htmlSettings())
	h := health(t, a)
	if h.IndexedCount != 2 || h.FailedCount != 0 || h.Coverage != recall.IndexComplete || h.Diagnostics["chunk_count"] != 5 {
		t.Fatalf("health: %#v", h)
	}
	for _, q := range []string{"orchard", "sunflower", "Carrots", "Cellar", "turnips"} {
		resp := search(t, a, q)
		if len(resp.Candidates) != 1 {
			t.Fatalf("query %q: %#v", q, resp)
		}
		c := resp.Candidates[0]
		if c.SourceRecordID != "report.html" || c.ExcerptKind != recall.ExcerptMatched || !strings.Contains(strings.ToLower(c.Excerpt), strings.ToLower(q)) {
			t.Errorf("query %q: %#v", q, c)
		}
		if c.Metadata["doc_title"] != "Field & Garden Report" {
			t.Errorf("title: %v", c.Metadata)
		}
	}
	if len(search(t, a, "Field").Candidates) == 0 {
		t.Error("HTML title is not searchable")
	}
	for _, q := range []string{"csspoison", "scriptpoison", "svgpoison", "base64poison", "hiddenpoison", "ariapoison", "displaypoison", "visibilitypoison", "mathpoison", "templatepoison", "noscriptpoison", "iframepoison", "hiddenheading"} {
		if resp := search(t, a, q); len(resp.Candidates) > 0 {
			t.Errorf("leaked %q", q)
		}
	}
	c := search(t, a, "turnips").Candidates[0]
	if c.Locator.Local != "report.html#L28-L29" || c.Metadata["anchor"] != "storage" {
		t.Fatalf("source locator/anchor: %#v", c)
	}
	for _, detail := range []recall.DetailLevel{recall.DetailFull, recall.DetailExcerpt, recall.DetailSummary, recall.DetailContext} {
		got, err := a.Expand(context.Background(), expandReq(c.Locator, detail, 0))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.Content, "Cellar instructions") || strings.Contains(got.Content, "<") || strings.Contains(got.Content, "poison") {
			t.Errorf("detail %s: %q", detail, got.Content)
		}
	}
	full, err := a.Expand(context.Background(), expandReq(c.Locator, recall.DetailFull, 0))
	if err != nil {
		t.Fatal(err)
	}
	if full.Content != "### Storage\nCellar instructions\n\nKeep turnips cool." {
		t.Fatalf("full: %q", full.Content)
	}
	bounded, err := a.Expand(context.Background(), expandReq(c.Locator, recall.DetailContext, 80))
	if err != nil || len(bounded.Content) > 80 || !bounded.Truncated || bounded.TruncationBoundary != "budget_bytes" {
		t.Fatalf("budget: %#v, %v", bounded, err)
	}
}

func TestHTMLLargeMarkupAndBoundedText(t *testing.T) {
	root := writtenCorpus(t, map[string]string{
		"image.html": "<h1>Pictures</h1><img alt='Photograph' src='data:image/png;base64," + strings.Repeat("A", 600000) + "'><p>" + strings.Repeat("garden ", 720) + "rareorchid.</p>",
	})
	// Under both the default limit and a deliberately small extracted-text cap.
	for _, cap := range []int{1 << 20, 6000} {
		settings := htmlSettings()
		settings["max_file_bytes"] = cap
		a, _ := newAdapter(t, root, settings)
		h := health(t, a)
		if h.IndexedCount != 1 || h.FailedCount != 0 {
			t.Fatalf("cap %d: %#v", cap, h)
		}
		c := search(t, a, "rareorchid").Candidates[0]
		if c.ExcerptKind != recall.ExcerptMatched || !strings.Contains(c.Excerpt, "rareorchid") || strings.Contains(c.Excerpt, "base64") {
			t.Fatal(c)
		}
		full, err := a.Expand(context.Background(), expandReq(c.Locator, recall.DetailFull, 0))
		if err != nil || !strings.Contains(full.Content, "rareorchid") || strings.Contains(full.Content, "data:") {
			t.Fatalf("expand: %#v %v", full, err)
		}
	}
	settings := htmlSettings()
	settings["max_raw_bytes"] = 100
	a, _ := newAdapter(t, root, settings)
	if h := health(t, a); h.FailedCount != 1 || !strings.Contains(strings.Join(h.Diagnostics["failures"].([]string), " "), "max_raw_bytes") {
		t.Fatal(h)
	}
	settings = htmlSettings()
	settings["max_file_bytes"] = 100
	a, _ = newAdapter(t, root, settings)
	if h := health(t, a); h.FailedCount != 1 || !strings.Contains(strings.Join(h.Diagnostics["failures"].([]string), " "), "max_file_bytes") {
		t.Fatal(h)
	}
}

func TestHTMLNoIndexAndSelectionSurviveReload(t *testing.T) {
	root := writtenCorpus(t, map[string]string{
		"report.md": "# Canonical\nmarkdownwinner", "report.html": "<h1>Render</h1>htmloser", "only.htm": "<h1>Only</h1>lonehtml",
		"secret.html":    "<head><meta name='robots' content='follow, NOINDEX'></head><p>secrettoken</p>",
		"mockups/a.html": "<p>mockuptoken</p>", "nested/mockups/a.md": "# mockuptoken", "x.template.html": "<p>templatetoken</p>",
	})
	settings := htmlSettings()
	settings["sibling_preference"] = []any{".md", ".html"}
	settings["exclude_globs"] = []any{"**/mockups/**", "*.template.html"}
	a, workdir := newAdapter(t, root, settings)
	check := func(a *docs.Adapter) {
		t.Helper()
		h := health(t, a)
		if h.IndexedCount != 2 || h.FailedCount != 0 || h.Coverage != recall.IndexComplete || h.Status != recall.HealthHealthy || h.Diagnostics["excluded_files"] != 3 {
			t.Fatalf("health: %#v", h)
		}
		if !strings.Contains(strings.Join(h.Diagnostics["excluded_files_sample"].([]string), " "), "secret.html: robots noindex") {
			t.Fatal(h.Diagnostics)
		}
		for _, q := range []string{"htmloser", "secrettoken", "mockuptoken", "templatetoken"} {
			if len(search(t, a, q).Candidates) > 0 {
				t.Errorf("excluded %s matched", q)
			}
		}
		for _, q := range []string{"markdownwinner", "lonehtml"} {
			if len(search(t, a, q).Candidates) != 1 {
				t.Errorf("%s not indexed", q)
			}
		}
	}
	check(a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := docs.New()
	defer reopened.Close()
	if _, err := reopened.Initialize(context.Background(), config(root, workdir, settings)); err != nil {
		t.Fatal(err)
	}
	check(reopened)
	// A live noindex change invalidates expansion; a rebuild admits it again.
	if err := os.WriteFile(filepath.Join(root, "only.htm"), []byte("<meta name='robots' content='noindex'><p>lonehtml</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	loc := recall.Locator{SourceID: "docs", Local: "only.htm#L1-L1"}
	if _, err := reopened.Expand(context.Background(), expandReq(loc, recall.DetailFull, 0)); err == nil {
		t.Fatal("expanded a newly noindex document")
	}
	if _, err := reopened.Refresh(context.Background(), protocol.RefreshParams{}); err != nil {
		t.Fatal(err)
	}
	if len(search(t, reopened, "lonehtml").Candidates) != 0 || health(t, reopened).FailedCount != 0 {
		t.Fatal("refresh did not exclude noindex")
	}
	if err := os.WriteFile(filepath.Join(root, "only.htm"), []byte("<h1>Only</h1><p>restoredhtml</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Refresh(context.Background(), protocol.RefreshParams{}); err != nil {
		t.Fatal(err)
	}
	if len(search(t, reopened, "restoredhtml").Candidates) != 1 {
		t.Fatal("refresh did not restore a newly indexable page")
	}
}

func TestHTMLDetailLimitsUseExtractedLines(t *testing.T) {
	body := "<h1>Long section</h1>" + strings.Repeat("<p>Some visible evidence.</p>", 40) + "<h2>Neighbor</h2><p>adjacentproof</p>"
	a, _ := newAdapter(t, writtenCorpus(t, map[string]string{"long.html": body}), htmlSettings())
	c := search(t, a, "visible").Candidates[0]
	excerpt, err := a.Expand(context.Background(), expandReq(c.Locator, recall.DetailExcerpt, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !excerpt.Truncated || excerpt.TruncationBoundary != "excerpt_lines" || strings.Contains(excerpt.Content, "adjacentproof") {
		t.Fatalf("excerpt: %#v", excerpt)
	}
}

func TestHTMLEmptyHeadingDoesNotPanic(t *testing.T) {
	a, _ := newAdapter(t, writtenCorpus(t, map[string]string{"empty.html": "<title>Searchable title</title><h1></h1>"}), htmlSettings())
	if len(search(t, a, "Searchable").Candidates) != 1 {
		t.Fatal("empty heading lost the document title")
	}
}

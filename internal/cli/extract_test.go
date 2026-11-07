package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/app"
	"github.com/marcus/recall/internal/cli"
)

func TestExtractHTMLWithoutConfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "report.html")
	run := func(source string) app.ExtractedHTML {
		t.Helper()
		if err := os.WriteFile(file, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := cli.Run(context.Background(), cli.Env{Args: []string{"extract", file, "--json"}, Stdout: &stdout, Stderr: &stderr})
		if code != 0 {
			t.Fatalf("exit %d: %s", code, &stderr)
		}
		var out app.ExtractedHTML
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := run(`<title>Report &amp; plan</title><p>Preamble</p><h2 id="next">Next steps</h2><p>Visible text</p><div data-noindex="false"><h2>Secret</h2>Hidden words</div><script>script text</script>`)
	if out.Title != "Report & plan" || len(out.Headings) != 2 || out.Headings[1].Anchor != "next" || !strings.Contains(out.Text, "Visible text") || strings.Contains(out.Text, "Hidden") || strings.Contains(out.Text, "script text") || out.ExtractionVersion == 0 {
		t.Fatalf("bad extraction: %+v", out)
	}
	out = run(`<title>Private</title><meta name="robots" content="noindex"><h1>Hidden</h1>`)
	if !out.NoIndex || out.Title != "" || out.Text != "" || len(out.Headings) != 0 {
		t.Fatalf("noindex leaked: %+v", out)
	}
}

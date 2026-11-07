package cli

import (
	"fmt"

	"github.com/marcus/recall/internal/app"
)

const extractHelp = `usage: recall extract <file.html> [--json]

Extract title, heading sections, source lines, anchors and text using Recall's
HTML parser. Excludes data-noindex and hidden subtrees. A robots noindex file
returns noindex=true with empty text and metadata. No profile or index is used.
Input is a regular UTF-8 HTML file, bounded to 16 MiB. JSON extractionVersion
identifies the parser contract; headings includes a preamble with level 0.

flags:
  --json    emit the extracted projection as JSON
`

func runExtract(env Env, args []string) int {
	fs := newFlagSet("extract")
	asJSON := fs.Bool("json", false, "emit JSON")
	if ok, code := parse(env, fs, extractHelp, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return usageErr(env, extractHelp, fmt.Errorf("extract takes exactly one HTML file"))
	}
	out, err := app.ExtractHTMLFile(fs.Arg(0))
	if err != nil {
		fail(env, err)
		return ExitError
	}
	if *asJSON {
		return report(env, emitJSON(env.Stdout, out))
	}
	if out.NoIndex {
		writeTo(env.Stdout, "noindex\n")
	} else {
		writeTo(env.Stdout, out.Title+"\n\n"+out.Text+"\n")
	}
	return ExitOK
}

# Documents adapter

The built-in `documents` adapter searches a local corpus with a heading-based lexical index. Queries return pointers to original files; expand reads those files again. The default extensions remain `.md` and `.markdown`, with the existing Markdown parser and ranking behavior.

## HTML sources

Add `.html` or `.htm` to a source's extensions to read HTML as structured text:

```toml
[sources.settings]
extensions = [".md", ".markdown", ".html", ".htm"]
max_file_bytes = 1048576
max_raw_bytes = 16777216
exclude_dirs = [".*", "node_modules"]
exclude_nested_repos = true
exclude_globs = ["**/mockups/**", "*.template.html"]
sibling_preference = [".md", ".html", ".htm"]
```

HTML is parsed with `golang.org/x/net/html`, including its recovery of omitted closing tags. Paragraphs and block elements produce text breaks, lists produce `- ` lines, and table rows produce `cell | cell` lines. Image alt text, figure captions, summaries, and details content are searchable. The title comes from `<title>`, then the first visible `h1`, then the file name. Title text is searchable even when it differs from the first heading.

Scripts, stylesheets, SVG, MathML, templates, noscript blocks, iframes, and head content other than the title are excluded. Link destinations, image sources, and other attribute values are not text, so embedded `data:` images do not enter the index. Subtrees with `data-noindex`, `hidden`, `aria-hidden="true"`, inline `display: none`, or inline `visibility: hidden`/`collapse` are skipped. The presence of `data-noindex` opts out the entire subtree, including headings, image alt text, lists, tables, title metadata, and robots directives; its value does not matter, so `data-noindex="false"` and `data-noindex=""` also opt out. This lets a visible mock screen stay available to readers and screen readers while keeping it out of retrieval. Extraction does not execute JavaScript, load external resources, or evaluate stylesheet selectors. Details content is included even when its disclosure is closed.

Subtree exclusions apply to both extracted text and metadata in the parsed HTML tree. Normal HTML recovery still applies: a heading that implicitly closes a marked paragraph is outside that paragraph, and invalid text inside a table can be moved outside the table by the parser. Use a valid enclosing element such as `<div data-noindex>…</div>` around a mock screen. Skipped content contributes no sections or terms; surrounding sections retain their original source-line positions.

A robots meta directive containing `noindex` excludes the file. It is not an indexing failure and does not degrade complete coverage. `recall doctor` reports the exclusion count and a bounded sample of paths and reasons. `recall sources --json` exposes the documents health diagnostics `excluded_files`, `excluded_files_sample`, `skipped_dirs`, and `skipped_dirs_sample`. Noindex exclusions belong to the published generation; a stale generation requires `recall refresh` to update that list.

Documents index format 5 rebuilds older generations automatically on initialization, including unchanged HTML files indexed before subtree opt-outs were supported. Adding or removing an opt-out after indexing takes effect in retrieval after `recall refresh`; expansion re-extracts the current source.

## Chunks, locators, and expansion

Each visible `h1` through `h6` opens a section, and heading levels form its heading path. Visible text before the first heading forms a preamble. A section starts on the original heading start-tag line and ends on the line before the next heading, or at the end of the source. Lines are 1-based and inclusive. Heading start tags spanning several lines retain their original first line.

Locators retain the original path and source lines, for example `docs:report.html#L21-L27`. A heading's `id`, when present, is exposed as candidate metadata `anchor`, alongside `path`, `heading_path`, `start_line`, and `end_line`. A surface can use the path and anchor to offer `report.html#harvest`. Headings sharing a source line necessarily share or overlap line ranges; a line-based locator cannot distinguish those sections individually.

HTML query excerpts and every expansion detail level return extracted text, including Markdown headings, list markers, and pipe-separated table rows. They never return the source markup. Excerpt and context line limits count extracted lines, while provenance names original source ranges. A range selects intersecting sections; context adds neighboring extracted lines. The same summary and byte-budget limits used for Markdown apply. Expansion re-reads the file, reports a changed file revision, and refuses a file that is now noindex or exceeds the configured size limits.

## Corpus boundaries and size limits

| Setting | Default | Meaning |
|---|---|---|
| `max_file_bytes` | 1 MiB | Maximum extracted HTML Markdown bytes, including title and separators; maximum raw file bytes for Markdown and other text extensions. |
| `max_raw_bytes` | 16 MiB | Maximum raw HTML bytes read or parsed, including embedded assets. The read remains bounded if a file grows after stat. |
| `exclude_globs` | `[]` | Root-relative path globs. `*`, `?`, and character classes use Go path glob syntax; a whole-segment `**` matches zero or more directories. |
| `sibling_preference` | `[]` | Ordered extension preference for regular, extension-eligible files with the same directory and stem. |

A slashless glob such as `*.template.html` matches at the corpus root; `**/*.template.html` matches at every depth. `**/mockups/**` excludes a `mockups` directory at any depth. Directory exclusions prune the walk and appear under `skipped_dirs`; individual file exclusions appear under `excluded_files`. Existing `exclude_dirs` continues to match directory basenames.

Sibling selection runs after path exclusions and before content extraction. With `[".md", ".html"]`, `report.md` wins over `report.html`, while a lone HTML file remains eligible. If a preferred sibling is present but fails extraction or declares noindex, there is no fallback to another rendering. With no preferred sibling present, all otherwise eligible files remain eligible. Only extensions declared in `extensions` participate. Preference order is significant; extension spelling is case-insensitive.

These settings participate in the settings digest. Changing one rebuilds the generation during initialization. Index format 4 rebuilds older generations automatically, including Markdown-only corpora; no manual deletion is needed.

## Shared Go API

`github.com/marcus/recall/pkg/document` is importable by external adapters and commands:

- `Read(reader, maxRawBytes)` bounds the raw read and validates UTF-8 text.
- `IsHTML(path)` recognizes `.html` and `.htm`, case-insensitively.
- `ExtractHTML(data, filename, maxRawBytes, maxFileBytes)` returns `HTML` or `ErrNoIndex`. A zero raw limit uses 16 MiB; a zero extracted-text limit disables that limit for callers that enforce their own boundary.
- `HTML` exposes `Title`, `SourceLines`, and `Chunks`. Each `Chunk` carries `Heading`, `Level`, `HeadingPath`, `Anchor`, `StartLine`, `EndLine`, and `Body`; `Chunk.Text()` renders a section. `HTML.Markdown()` renders the document title followed by those same sections for Markdown-aware consumers.
- `Selection.Validate()`, `DirectoryReason(root, rel)`, and `Select(eligiblePaths)` share directory pruning, globs, and sibling preference. `GlobReason(rel)` and `MatchGlob(pattern, rel)` expose the path-matching rule. `Select` returns sorted selected paths and exclusions without mutating its input. Callers must enumerate only regular, extension-eligible files and leave the explicitly configured root unpruned.

The documents adapter uses this API for HTML indexing, query excerpts, and expansion. Semantic derived-text synchronization is separate work; the qmd adapter's behavior is unchanged.

## Extracting without an index

`recall extract report.html --json` reads one regular HTML file through the same bounded extractor, without loading configuration or writing an index. It returns `title`, `text`, `headings`, `noindex` and `extractionVersion`. Each heading section has `heading`, `level`, optional `anchor`, `text`, and inclusive original `startLine` / `endLine`; a preamble has level 0 and an empty heading. Robots noindex returns empty metadata and text with `noindex: true`. The raw UTF-8 input limit is 16 MiB; invalid, binary or nonregular inputs fail with exit 1. Human output carries the title and extracted text, or `noindex`.

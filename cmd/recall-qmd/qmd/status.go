package qmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/marcus/recall/pkg/protocol"
)

// This file parses qmd's three human-readable reports: `qmd status`, `qmd
// collection show`, and `qmd --version`.
//
// Parsing text is a liability and it is a deliberate one. qmd 2.5.3 accepts
// `--format json` on all three and ignores it, so the alternative to parsing
// is reporting nothing — no index identity, no counts, no model identity, and
// no way to check that the configured collection indexes the directory this
// source was configured for. Every field is therefore optional in the parse and
// absent in the result when qmd's wording changes: a count that could not be
// read leaves coverage unknown and degrades the source, and a collection path
// that could not be read makes it unavailable. A wording change surfaces as a
// source that cannot confirm itself, never as one that quietly answers.

// indexReport is what `qmd status` says about the index this instance opened.
type indexReport struct {
	// IndexPath is the SQLite file qmd opened. It is used only to derive an
	// opaque store identity; it never reaches a candidate, a diagnostic, or a
	// transcript.
	IndexPath string

	// Documents and Vectors are index-wide counts across every collection.
	Documents int
	Vectors   int
	HasCounts bool

	// Collection is this source's collection as the index reports it.
	Collection    collectionStatus
	HasCollection bool

	// Models identify the three GGUF models qmd would use. They are part of
	// index_config: a model change is a scoring change, and a scoring change
	// that nothing records would be credited to whatever else an evaluation had
	// under test.
	Embedding string
	Reranker  string
	Expansion string
}

type collectionStatus struct {
	Name     string
	Pattern  string
	Files    int
	HasFiles bool
}

// collectionReport is what `qmd collection show` says about one collection.
type collectionReport struct {
	Name string
	// Path is the directory the collection indexes, as qmd resolved it. It is
	// the value the configured location is checked against.
	Path     string
	Pattern  string
	Included bool
}

var (
	collectionHeader = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*) \(qmd://([A-Za-z0-9][A-Za-z0-9._-]*)/\)$`)
	firstInteger     = regexp.MustCompile(`\d+`)
)

// parseStatus reads `qmd status`, keeping only what the named collection needs.
func parseStatus(text, collection string) indexReport {
	var report indexReport
	var current string
	for _, raw := range strings.Split(text, "\n") {
		line := scrub(raw)
		if line == "" {
			continue
		}
		if match := collectionHeader.FindStringSubmatch(line); match != nil && match[1] == match[2] {
			current = match[1]
			if current == collection {
				report.HasCollection = true
				report.Collection.Name = current
			}
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "Index":
			if report.IndexPath == "" {
				report.IndexPath = value
			}
		case "Total":
			if n, ok := leadingInt(value); ok {
				report.Documents, report.HasCounts = n, true
			}
		case "Vectors":
			if n, ok := leadingInt(value); ok {
				report.Vectors = n
			}
		case "Pattern":
			if current == collection {
				report.Collection.Pattern = value
			}
		case "Files":
			if current == collection {
				if n, ok := leadingInt(value); ok {
					report.Collection.Files, report.Collection.HasFiles = n, true
				}
			}
		case "Embedding":
			report.Embedding = modelName(value)
		case "Reranking":
			report.Reranker = modelName(value)
		case "Generation":
			report.Expansion = modelName(value)
		}
	}
	return report
}

// parseCollection reads `qmd collection show <name>`.
//
// qmd exits 0 when the collection does not exist and says so in prose, so this
// is where that becomes a source failure. Returning an empty report instead
// would let a search run against every collection in the index.
func parseCollection(text, want string) (collectionReport, error) {
	report := collectionReport{Included: true}
	for _, raw := range strings.Split(text, "\n") {
		line := scrub(raw)
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "Collection":
			report.Name = value
		case "Collection not found":
			return collectionReport{}, fmt.Errorf(
				"%w: qmd holds no collection named %q", protocol.ErrSourceUnavailable, sanitizeLine(want))
		case "Path":
			report.Path = value
		case "Pattern":
			report.Pattern = value
		case "Include":
			report.Included = strings.HasPrefix(strings.ToLower(value), "yes")
		}
	}
	switch {
	case report.Name == "":
		return collectionReport{}, fmt.Errorf(
			"%w: qmd collection show named no collection", errBrokenContract)
	case report.Name != want:
		return collectionReport{}, fmt.Errorf(
			"%w: qmd collection show %q reported collection %q",
			errBrokenContract, sanitizeLine(want), sanitizeLine(report.Name))
	case report.Path == "":
		// Without it there is nothing to compare the configured location
		// against, and the whole point of the check is that a collection
		// indexing another directory must not answer for this source.
		return collectionReport{}, fmt.Errorf(
			"%w: qmd collection show %q reported no path", errBrokenContract, sanitizeLine(want))
	}
	return report, nil
}

// parseVersion reads `qmd --version`, which prints one line: "qmd 2.5.3".
//
// The program name is dropped, because the value is reported as `qmd=<version>`
// and "qmd=qmd 2.5.3" reads as a mistake. What is left is whatever qmd said,
// unparsed: a version scheme this adapter validated would be a version scheme it
// could disagree with.
func parseVersion(text string) string {
	for _, raw := range strings.Split(text, "\n") {
		line := scrub(raw)
		if line == "" {
			continue
		}
		if rest := strings.TrimSpace(strings.TrimPrefix(line, "qmd")); rest != "" && rest != line {
			line = rest
		}
		if len(line) > 64 {
			line = line[:cutRunes(line, 64)]
		}
		return line
	}
	return ""
}

func leadingInt(value string) (int, bool) {
	match := firstInteger.FindString(value)
	if match == "" {
		return 0, false
	}
	n, err := strconv.Atoi(match)
	if err != nil {
		return 0, false
	}
	return n, true
}

// modelName reduces a model reference to its last path segment.
//
// qmd reports a Hugging Face URL. The repository name identifies the model and
// the host does not, and keeping only the segment means nothing that looks like
// a fetchable location lands in a health report a person reads.
func modelName(value string) string {
	value = strings.TrimSuffix(sanitizeLine(value), "/")
	if value == "" {
		return ""
	}
	return path.Base(value)
}

// storeIdentity is an equality-only, opaque name for the store this instance
// opened: qmd's index file plus the collection inside it.
//
// Both halves are needed and neither is sufficient. Two sources over one index
// but different collections are two corpora and must not compare equal; two
// sources naming one collection of one index are the same records reaching the
// core twice, which is the duplicate-instance defect the check exists to catch.
// The value is hashed because it is only ever compared for equality, and
// because a raw path would put a home directory into a diagnostic, a log, and a
// committed conformance transcript.
func storeIdentity(indexPath, collection string) string {
	if indexPath == "" || collection == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(canonical(indexPath) + "\x00" + collection))
	return "qmd:" + hex.EncodeToString(sum[:8])
}

// indexConfig identifies the retrieval configuration a set of answers was
// produced under.
//
// qmd's models and blending can change on a package bump, which is exactly what
// this field exists to make visible: without it an evaluation comparing two
// runs would credit a model upgrade to whatever it had under test. The mode is
// part of it for the same reason — bm25 and full are different retrieval
// systems, and comparing a run of one against a run of the other by accident is
// the mistake this string prevents.
func indexConfig(version string, report indexReport, set Settings) string {
	parts := []string{
		"qmd=" + orNone(version),
		set.settingsDigest(),
		"pattern=" + orNone(report.Collection.Pattern),
	}
	if set.Mode.Embeds() {
		parts = append(parts, "embed="+orNone(report.Embedding))
	}
	if set.Mode.Reranks() {
		parts = append(parts, "rerank="+orNone(report.Reranker))
	}
	if set.Mode.Expands() {
		parts = append(parts, "expand="+orNone(report.Expansion))
	}
	return strings.Join(parts, " ")
}

// indexModel is the embedding model of the answers a mode produces. A mode that
// consults no embeddings declares none rather than naming a model it did not
// use.
func indexModel(report indexReport, set Settings) string {
	if !set.Mode.Embeds() {
		return ""
	}
	return report.Embedding
}

// sourceWatermark is freshness evidence for the corpus, as qmd reports it.
//
// It is counts, and the limits of that are worth stating rather than hiding.
// docs/writing-an-adapter.md asks for a watermark derived from the source and
// stable across a rebuild, and warns that a count alone does not move when one
// record replaces another. This one is stable across a rebuild of an unchanged
// corpus and identical on two machines indexing the same corpus, which are the
// two properties a caller comparing watermarks depends on — but qmd exposes no
// content digest for a collection, and computing one here would mean walking
// the corpus this adapter exists to delegate. The gap is declared in doc.go and
// in the freshness policy: same-size, same-count content edits are invisible to
// it until a refresh moves the vector count.
func sourceWatermark(report indexReport, set Settings) string {
	files := "?"
	if report.Collection.HasFiles {
		files = strconv.Itoa(report.Collection.Files)
	}
	return fmt.Sprintf("collection=%s files=%s pattern=%s",
		set.Collection, files, orNone(report.Collection.Pattern))
}

// indexWatermark is what the index holds, as distinct from what the corpus
// holds. The vector count is the half a rebuild moves first, and it is what
// separates "reindexed" from "reindexed and embedded".
func indexWatermark(report indexReport) string {
	if !report.HasCounts {
		return ""
	}
	return fmt.Sprintf("docs=%d vectors=%d", report.Documents, report.Vectors)
}

// indexGeneration names the published generation, as far as anything outside
// qmd can.
//
// qmd owns one SQLite file, publishes no generation pointer, and offers no
// generation identity to report — so this is a digest of what its index says
// about itself under the configuration in force. It changes when the counts,
// the models, the mode, or the collection boundary change, and it does not
// change when an unchanged corpus is reindexed. It cannot distinguish two
// different corpora that happen to produce the same counts under the same
// configuration, which is why the value is declared best-effort here and in
// doc.go rather than presented as the atomic generation identity the index
// obligations describe.
func indexGeneration(version string, report indexReport, set Settings) string {
	if !report.HasCounts {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		indexConfig(version, report, set),
		indexWatermark(report),
		sourceWatermark(report, set),
	}, "\x00")))
	return "qmd-gen-" + hex.EncodeToString(sum[:6])
}

// coverageOf decides what fraction of the corpus a search in this mode could
// have seen, from one status report.
//
// Health and search consume the same function over the same snapshot, because a
// search reporting partial beside a health probe reporting complete cannot both
// be true and the core reads both.
func coverageOf(report indexReport, set Settings) (recallCoverage, string) {
	switch {
	case !report.HasCollection:
		return coverageUnknown, "qmd status does not list this collection"
	case !report.HasCounts || !report.Collection.HasFiles:
		return coverageUnknown, "qmd status did not report index counts"
	case report.Collection.Files == 0:
		// An empty collection is a complete boundary over nothing. Saying
		// unknown here would make every honest "no such document" degrade.
		return coverageComplete, ""
	case set.Mode.Embeds() && report.Vectors == 0:
		return coverageNone, "this mode searches embeddings and the index holds none; run recall refresh"
	case set.Mode.Embeds() && report.Vectors < report.Documents:
		// qmd embeds chunks, so a fully embedded corpus holds at least one
		// vector per document. Fewer means documents exist that no vector
		// represents, and a vector or hybrid search cannot have seen them.
		return coveragePartial, fmt.Sprintf(
			"%d indexed documents and %d vector chunks: not every document is embedded",
			report.Documents, report.Vectors)
	default:
		return coverageComplete, ""
	}
}

// recallCoverage is the internal three-and-a-half-state answer coverageOf
// gives. It is mapped to the protocol's coverage and outcome vocabularies by
// the callers, which need different projections of it.
type recallCoverage int

const (
	coverageComplete recallCoverage = iota
	coveragePartial
	coverageUnknown
	// coverageNone is a corpus this mode cannot search at all. It is not a
	// partial view: a vector search against an index holding no vectors sees
	// nothing, and reporting that as a partial answer would let an empty result
	// stand as evidence.
	coverageNone
)

func orNone(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

// verifyCollection asks qmd which directory the configured collection indexes,
// and refuses to serve anything when it is not the configured location.
//
// The check is repeated per operation rather than cached from a prior health
// probe on purpose, and the td adapter's history is the reason: a source that
// trusted an earlier probe kept answering after its store had moved underneath
// it. `qmd collection add` can be run at any time and re-point a collection at
// another directory, and this process is long-lived.
//
// It is the smallest probe that makes an answer trustworthy, and it is the whole
// of what expansion needs: a locator's path is collection-relative, so a
// collection pointing at another tree would resolve the same relative path
// inside a different corpus and return a file that is not the one that ranked.
func (a *Adapter) verifyCollection(ctx context.Context, want string) (collectionReport, error) {
	set, _, _, _, err := a.session()
	if err != nil {
		return collectionReport{}, err
	}
	args := []string{"collection", "show", set.Collection}
	res, err := a.run(ctx, args...)
	if err != nil {
		return collectionReport{}, err
	}
	text, err := decodeText(res, args...)
	if err != nil {
		return collectionReport{}, err
	}
	collection, err := parseCollection(text, set.Collection)
	if err != nil {
		return collectionReport{}, err
	}
	if err := checkCollectionPath(collection, want); err != nil {
		return collectionReport{}, err
	}
	return collection, nil
}

// probeIndex verifies the collection and then reads the index counts a search
// and a health probe both need.
func (a *Adapter) probeIndex(ctx context.Context, want string) (indexReport, collectionReport, error) {
	set, _, _, _, err := a.session()
	if err != nil {
		return indexReport{}, collectionReport{}, err
	}
	collection, err := a.verifyCollection(ctx, want)
	if err != nil {
		return indexReport{}, collectionReport{}, err
	}

	args := []string{"status"}
	res, err := a.run(ctx, args...)
	if err != nil {
		return indexReport{}, collection, err
	}
	text, err := decodeText(res, args...)
	if err != nil {
		return indexReport{}, collection, err
	}
	report := parseStatus(text, set.Collection)
	if report.Collection.Pattern == "" {
		report.Collection.Pattern = collection.Pattern
	}
	if !report.HasCollection {
		return report, collection, fmt.Errorf(
			"%w: qmd status does not list collection %q; the index does not hold it",
			protocol.ErrSourceUnavailable, sanitizeLine(set.Collection))
	}
	return report, collection, nil
}

// checkCollectionPath refuses a collection that indexes a directory other than
// the one this source was configured for.
//
// It is the store-identity rule applied to a store this adapter does not own:
// identity comes from what was opened, never from what configuration said. A
// collection re-pointed at another tree would otherwise let this source return
// candidates naming files from a corpus it was never configured to read, with
// every locator, every relative path, and every expansion silently answering
// for the wrong tree.
func checkCollectionPath(collection collectionReport, want string) error {
	if want == "" {
		return fmt.Errorf("%w: this source names no corpus location", protocol.ErrSourceUnavailable)
	}
	got := canonical(collection.Path)
	if got == canonical(want) {
		return nil
	}
	// Named by base name only. A diagnostic does not need an absolute path, and
	// this one travels into health reports and committed transcripts.
	return fmt.Errorf("%w: qmd collection %q indexes %q, but this source is configured at %q; "+
		"a search would answer from a corpus this source was not configured for",
		protocol.ErrSourceUnavailable, sanitizeLine(collection.Name),
		labelOf(got), labelOf(want))
}

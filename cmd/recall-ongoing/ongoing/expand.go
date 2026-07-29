package ongoing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// idPrefix is what every ongoing project id starts with. It is what makes a
// reference this adapter cannot read — locator_unknown — a different fact from
// one it can read that no longer resolves, which is locator_expired.
const idPrefix = "project_"

// Expand retrieves the evidence behind a locator.
//
// The local part is the project's own id, which ongoing derives from the
// repository's canonical path. That is what makes it stable across the daily
// rescan: the scan rewrites every measurement and leaves the id alone. A
// project that moves on disk gets a new id, which is the correct outcome —
// under ongoing's own identity rules it is a different project, and the old
// locator has expired rather than quietly resolving to something else.
//
// The catalog is read live, so an expansion always shows current detail rather
// than whatever a search saw a moment ago.
func (a *Adapter) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	set, tr, _, _, err := a.session()
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	id, err := parseLocal(req.Locator.Local)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	cat, err := a.fetch(ctx, tr, set)
	if err != nil {
		return recall.ExpandResponse{}, err
	}

	proj, ok := cat.find(id)
	if !ok {
		// The project is not in the catalog this instance is serving. Either it
		// was removed, or it moved and was recatalogued under a new id. Both
		// are the same incompatible change from the caller's side, and neither
		// permits returning a nearby project instead.
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired,
			"the catalog at revision %s holds no project %s", cat.revision(), id)
	}
	if !set.keeps(proj) {
		// The project exists but is outside this source's configured views. It
		// is not this source's record to expand, and saying it expired is the
		// honest answer: this locator names something this instance does not
		// serve.
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired,
			"project %s is outside this source's configured views", id)
	}

	content := render(proj, cat, req.Detail)
	truncated, boundary := false, ""
	if req.Budget > 0 && int64(len(content)) > req.Budget {
		content = clipBytes(content, int(req.Budget))
		truncated, boundary = true, "budget_bytes"
	}
	return recall.ExpandResponse{
		Content:            content,
		SourceRevision:     cat.revision(),
		Truncated:          truncated,
		TruncationBoundary: boundary,
		Provenance:         fmt.Sprintf("%s (%s)", oneLine(proj.CanonicalPath), proj.ID),
	}, nil
}

// parseLocal reads the adapter-local part of a locator.
func parseLocal(local string) (string, error) {
	if !strings.HasPrefix(local, idPrefix) || len(local) <= len(idPrefix) {
		return "", protocol.Errorf(protocol.CodeLocatorUnknown,
			"%q is not an ongoing locator, want %s<id>", local, idPrefix)
	}
	return local, nil
}

// render turns a project into evidence at one detail level.
//
// The levels widen rather than reshape: each one's output starts with the
// previous one's, so a caller comparing a summary against a full expansion sees
// added sections and not rewritten ones.
//
// Every value is written as a labelled field and every free-text field is
// collapsed onto one line first. A note or a commit subject is data; it must
// not be able to forge a section header in evidence a model reads.
func render(p *project, cat *catalog, detail recall.DetailLevel) string {
	var b strings.Builder
	writeIdentity(&b, p)

	switch detail {
	case recall.DetailSummary:
		return b.String()
	case recall.DetailExcerpt:
		writeDecisions(&b, p)
		writeReasons(&b, p)
		return b.String()
	case recall.DetailFull, recall.DetailContext:
		writeDecisions(&b, p)
		writeReasons(&b, p)
		writeEvidence(&b, p)
		writeMeasurements(&b, p)
		writeWarnings(&b, p)
		writeCatalogState(&b, cat, p)
	default:
		// An unknown level is not an invitation to guess how much to reveal.
		return b.String()
	}

	if detail == recall.DetailContext {
		writeTrends(&b, p)
	}
	return b.String()
}

// writeIdentity is the summary: what this project is and where it stands.
func writeIdentity(b *strings.Builder, p *project) {
	fmt.Fprintf(b, "%s\nproject at %s", title(p), p.CanonicalPath)
	if p.IsMissing {
		b.WriteString("\nmissing from disk")
	}
	if m := p.Metrics; m != nil {
		if m.Branch != "" {
			fmt.Fprintf(b, "\nbranch: %s", oneLine(m.Branch))
			if m.LatestCommitShortSha != "" {
				fmt.Fprintf(b, " @ %s", oneLine(m.LatestCommitShortSha))
			}
		}
		if m.LatestCommitAt != nil {
			fmt.Fprintf(b, "\nlatest commit: %s", m.LatestCommitAt.UTC().Format(time.RFC3339))
			if m.LatestCommitSubject != "" {
				fmt.Fprintf(b, " — %s", oneLine(m.LatestCommitSubject))
			}
		}
	}
	if views := p.memberViews(); len(views) > 0 {
		labels := make([]string, 0, len(views))
		for _, key := range views {
			labels = append(labels, viewLabels[key])
		}
		fmt.Fprintf(b, "\nclassifications: %s", strings.Join(labels, ", "))
	} else {
		b.WriteString("\nclassifications: none")
	}
}

// writeDecisions is the owner's own record: the fields nothing else in the
// corpus holds.
func writeDecisions(b *strings.Builder, p *project) {
	lines := make([]string, 0, 5)
	if note := oneLine(p.Note); note != "" {
		lines = append(lines, "note: "+note)
	}
	if p.Intent != nil && *p.Intent != "" {
		lines = append(lines, "intent: "+oneLine(*p.Intent))
	}
	if p.NextAction != nil && oneLine(*p.NextAction) != "" {
		lines = append(lines, "next action: "+oneLine(*p.NextAction))
	}
	if p.ReviewAfter != nil && *p.ReviewAfter != "" {
		lines = append(lines, "review after: "+oneLine(*p.ReviewAfter))
	}
	// Excitement and strategic importance are the owner's own 1-to-5 ratings.
	// They are the closest thing this catalog has to a judgement, and they are
	// reported as what they are — two separate answers to two separate
	// questions — rather than folded into one another or into anything else.
	if p.Excitement != nil {
		lines = append(lines, fmt.Sprintf("excitement: %d of 5", *p.Excitement))
	}
	if p.StrategicImportance != nil {
		lines = append(lines, fmt.Sprintf("strategic importance: %d of 5", *p.StrategicImportance))
	}
	if p.IsFavorite {
		lines = append(lines, "favorite: true")
	}
	if len(lines) > 0 {
		fmt.Fprintf(b, "\n\nDecisions:\n%s", strings.Join(lines, "\n"))
	}
}

// writeReasons lists why each classification held, in prose.
func writeReasons(b *strings.Builder, p *project) {
	views := p.memberViews()
	if len(views) == 0 {
		return
	}
	b.WriteString("\n\nAttention:")
	for _, key := range views {
		reasons := p.reasonsFor(key)
		if len(reasons) == 0 {
			fmt.Fprintf(b, "\n- %s", viewLabels[key])
			continue
		}
		for _, r := range reasons {
			fmt.Fprintf(b, "\n- %s: %s", viewLabels[key], oneLine(r.Message))
		}
	}
}

// writeEvidence restates each reason as the comparison ongoing actually made.
//
// The prose above is what a person reads; this is what an argument is had over.
// Carrying input, value, comparison, and threshold is the difference between a
// classification a reader can check and a label they have to trust.
func writeEvidence(b *strings.Builder, p *project) {
	views := p.memberViews()
	if len(views) == 0 {
		return
	}
	wrote := false
	for _, key := range views {
		for _, r := range p.reasonsFor(key) {
			if !wrote {
				b.WriteString("\n\nEvidence:")
				wrote = true
			}
			// Input and Comparison are ongoing's own words for what was
			// measured, and they land inside a section this adapter labels
			// "Evidence:". Without collapsing them a reason can forge another
			// section header and appear to be structure rather than quoted
			// content — the thing render's doc comment already promised.
			fmt.Fprintf(b, "\n- %s/%s %s = %s %s %s", oneLine(key), oneLine(r.Source),
				oneLine(r.Input), fmtValue(r.Value), oneLine(r.Comparison), fmtValue(r.Threshold))
		}
	}
}

// writeMeasurements is every collected metric this adapter carries, grouped by
// the collector that produced it and stamped with when that collector last ran.
// The stamp is not decoration: ongoing's own rules ignore a measurement older
// than 72 hours, so a number without its collection time is a number nobody can
// weigh.
func writeMeasurements(b *strings.Builder, p *project) {
	m := p.Metrics
	if m == nil {
		b.WriteString("\n\nMeasurements: none collected")
		return
	}
	b.WriteString("\n\nMeasurements:")

	git := fields{}
	git.count("commits", m.CommitCount)
	git.count("commits 7d", m.Commits7d)
	git.count("commits 30d", m.Commits30d)
	git.count("commits 90d", m.Commits90d)
	git.count("active days 30d", m.ActiveDays30d)
	git.count("contributors", m.ContributorCount)
	git.count("dirty files", m.DirtyFiles)
	git.count("ahead", m.AheadCount)
	git.count("behind", m.BehindCount)
	git.str("latest tag", m.LatestTag)
	git.str("head", m.HeadSha)
	writeGroup(b, "git", git, m.GitScannedAt)

	loc := fields{}
	loc.count("code", m.LocCode)
	loc.count("test", m.LocTest)
	loc.count("files", m.LocFiles)
	loc.str("dominant language", m.DominantLanguage)
	writeGroup(b, "loc", loc, m.LocScannedAt)

	td := fields{}
	td.count("open", m.TdOpenCount)
	td.count("in progress", m.TdInProgressCount)
	td.count("blocked", m.TdBlockedCount)
	td.count("review", m.TdReviewCount)
	td.count("stale", m.TdStaleCount)
	td.count("not closed", m.TdTotalNonClosedCount)
	writeGroup(b, "td", td, m.TdScannedAt)

	gh := fields{}
	gh.str("repository", repository(m))
	gh.str("visibility", m.GithubVisibility)
	gh.flag("archived", m.GithubIsArchived)
	gh.count("stars", m.GithubStars)
	gh.count("forks", m.GithubForks)
	gh.count("open issues", m.GithubOpenIssues)
	gh.count("open PRs", m.GithubOpenPrs)
	gh.count("external PRs", m.GithubExternalPrs)
	gh.when("oldest external PR", m.GithubOldestExternalPrAt)
	gh.count("merged PRs 30d", m.GithubMergedPrs30d)
	gh.count("external issues 30d", m.GithubExternalIssues30d)
	gh.when("latest release", m.GithubLatestReleaseAt)
	gh.str("latest release tag", m.GithubLatestReleaseTag)
	gh.str("CI", m.GithubCiState)
	gh.str("availability", m.GithubAvailability)
	writeGroup(b, "github", gh, m.GithubScannedAt)

	traffic := fields{}
	traffic.count("views", m.GithubTrafficViews)
	traffic.count("unique visitors", m.GithubTrafficUniqueVisitors)
	traffic.count("clones", m.GithubTrafficClones)
	traffic.str("availability", m.GithubTrafficAvailability)
	writeGroup(b, "traffic", traffic, nil)
}

// writeWarnings names the collectors that failed, so a reader knows which
// measurements above are missing rather than zero.
func writeWarnings(b *strings.Builder, p *project) {
	if len(p.Errors) == 0 {
		return
	}
	b.WriteString("\n\nCollector warnings:")
	for _, e := range p.Errors {
		fmt.Fprintf(b, "\n- %s: %s (%s)", oneLine(e.Collector), oneLine(e.Message),
			e.OccurredAt.UTC().Format(time.RFC3339))
	}
}

// writeCatalogState says how old the evidence above is, in the source's own
// terms. An expansion that did not carry its own freshness would invite a
// reader to treat a three-day-old scan as this morning's.
func writeCatalogState(b *strings.Builder, cat *catalog, p *project) {
	b.WriteString("\n\nCatalog:")
	fmt.Fprintf(b, "\nrevision: %s", cat.revision())
	if !p.LastSeenAt.IsZero() {
		fmt.Fprintf(b, "\nlast seen on disk: %s", p.LastSeenAt.UTC().Format(time.RFC3339))
	}
	if cat.Scan != nil {
		fmt.Fprintf(b, "\nlast scan: %s", cat.Scan.Status)
		if cat.Scan.FinishedAt != nil {
			fmt.Fprintf(b, ", finished %s", cat.Scan.FinishedAt.UTC().Format(time.RFC3339))
		}
	}
	if age, ok := cat.age(); ok {
		fmt.Fprintf(b, "\nage: %dh", int(age.Hours()))
		if cat.stale() {
			fmt.Fprintf(b, " (older than ongoing's %dh freshness rule)", int(StaleAfter.Hours()))
		}
	}
}

// writeTrends is the `context` level: the daily history the catalog keeps.
//
// It is the only history there is — eight numeric metrics, a few weeks deep —
// and it is what makes "rising" or "dormant" checkable rather than asserted.
func writeTrends(b *strings.Builder, p *project) {
	tracked := []string{
		"github_stars", "github_traffic_views", "github_traffic_clones",
		"github_open_issues", "github_open_prs", "loc_code",
	}
	wrote := false
	for _, metric := range tracked {
		series := p.snapshotSeries(metric)
		if len(series) < 2 {
			// One point is a value, not a trend, and it is already in the
			// measurements above.
			continue
		}
		if !wrote {
			b.WriteString("\n\nTrend:")
			wrote = true
		}
		first, last := series[0], series[len(series)-1]
		fmt.Fprintf(b, "\n- %s: %s → %s (%s to %s, %d points)",
			metric, fmtValue(first.Value), fmtValue(last.Value),
			first.CapturedOn, last.CapturedOn, len(series))
	}
	if !wrote {
		b.WriteString("\n\nTrend: no metric has more than one recorded day")
	}
}

// fields collects the "label: value" pairs of one measurement group, dropping
// every value the catalog does not have. A missing measurement is unknown, not
// zero.
type fields []string

func (f *fields) count(label string, value *int) {
	if value != nil {
		*f = append(*f, fmt.Sprintf("%s %d", label, *value))
	}
}

func (f *fields) str(label, value string) {
	if v := oneLine(value); v != "" {
		*f = append(*f, label+" "+v)
	}
}

func (f *fields) flag(label string, value *bool) {
	if value != nil && *value {
		*f = append(*f, label)
	}
}

func (f *fields) when(label string, value *time.Time) {
	if value != nil {
		*f = append(*f, label+" "+value.UTC().Format(time.RFC3339))
	}
}

// writeGroup emits one collector's line, or says it collected nothing. A group
// that is simply absent would read as a group with nothing to report.
func writeGroup(b *strings.Builder, name string, f fields, scannedAt *time.Time) {
	if len(f) == 0 {
		fmt.Fprintf(b, "\n%s: not collected", name)
		return
	}
	fmt.Fprintf(b, "\n%s: %s", name, strings.Join(f, ", "))
	if scannedAt != nil {
		fmt.Fprintf(b, " [collected %s]", scannedAt.UTC().Format(time.RFC3339))
	}
}

// clipBytes cuts at a rune boundary so a truncated expansion is still text.
func clipBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

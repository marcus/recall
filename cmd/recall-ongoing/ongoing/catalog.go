package ongoing

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// StaleAfter is how old a catalog may be before this source reports degraded.
//
// It is ongoing's own number, not one Recall picked: ATTENTION_THRESHOLDS
// declares 72 hours for every collector, and a measurement older than that
// satisfies no attention rule. A catalog whose last scan finished longer ago
// than this is therefore serving classifications its own product rules would
// refuse to compute, which is exactly the condition `degraded` exists to name.
// It is a constant rather than a setting because it is the source's policy;
// making it configurable here would let a Recall config quietly disagree with
// the system it is describing.
const StaleAfter = 72 * time.Hour

// viewKeys are ongoing's attention classifications, in the order its own
// dashboard lists them. Ranking and rendering walk this slice rather than a map
// so a candidate's views and reasons are ordered identically on every run.
var viewKeys = []string{"attention", "rising", "quickwin", "opportunity", "momentum", "dormant"}

// viewLabels render a classification for a human. The API's spelling is what
// travels in metadata — inventing a vocabulary the source does not use would
// make a candidate impossible to compare against ongoing's own UI — and these
// appear only in text a person reads.
var viewLabels = map[string]string{
	"attention":   "needs attention",
	"rising":      "rising",
	"quickwin":    "quick win",
	"opportunity": "opportunity",
	"momentum":    "momentum",
	"dormant":     "dormant",
}

func knownView(key string) bool { return viewLabels[key] != "" }

// catalog is GET /api/projects: ongoing's dashboard page model.
//
// Only the fields this adapter reads are declared. Unknown fields are ignored
// rather than rejected: the catalog is a source Recall does not own, and a
// field added on the ongoing side must not take this source down.
// vocabulary is every token this catalog's searchable text contains.
//
// It is built once per search and read as the source-wide membership test
// number-variant resolution needs: only a term this source spells nowhere is
// looked for under another number. Built once, and not probed per term, because
// this source keeps no index — weigh() re-tokenizes a project's whole field
// list on every call, so a per-term probe walked the entire catalog once for
// each term and once again for each spelling it tried.
func (c *catalog) vocabulary() func(string) bool {
	seen := map[string]struct{}{}
	for i := range c.Projects {
		for token := range weigh(&c.Projects[i]) {
			seen[token] = struct{}{}
		}
	}
	return func(term string) bool {
		_, ok := seen[term]
		return ok
	}
}

type catalog struct {
	GeneratedAt time.Time `json:"generatedAt"`
	HiddenCount int       `json:"hiddenCount"`

	// LoadError is the page model's own report that the catalog could not be
	// read. The route answers 503 in that case, but the field is part of the
	// contract and a 200 carrying one is still a source that did not answer.
	LoadError *string `json:"loadError"`

	// Scan is the latest scan run, or nil when the catalog has never been
	// scanned. It is the only complete source boundary this source has.
	Scan *scanRun `json:"scan"`

	Projects []project `json:"projects"`
}

// scanRun is one catalog refresh. Status is ongoing's own vocabulary:
// running, completed, failed, cancelled.
//
// StartedAt is carried for the case where Status is not "completed": a scan
// that started four minutes ago and one that has been running since Tuesday are
// the same degraded status and very different problems.
type scanRun struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	ErrorCount int        `json:"errorCount"`
}

const scanCompleted = "completed"

// project is one catalogued repository.
type project struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	RelativePath  string `json:"relativePath"`
	CanonicalPath string `json:"canonicalPath"`

	IsFavorite bool `json:"isFavorite"`
	IsMissing  bool `json:"isMissing"`

	// The owner's own decisions. These are the fields nothing else in the
	// corpus records, and the reason a project catalog is worth querying at
	// all rather than re-derived from git.
	Note                string  `json:"note"`
	Intent              *string `json:"intent"`
	NextAction          *string `json:"nextAction"`
	Excitement          *int    `json:"excitement"`
	StrategicImportance *int    `json:"strategicImportance"`
	ReviewAfter         *string `json:"reviewAfter"`

	// LastSeenAt is when a scan last found this repository where the catalog
	// says it is. It is per-record sighting evidence, and it is reported beside
	// the scan boundary rather than in place of it: a sighting inside an
	// unfinished pass confirms nothing.
	LastSeenAt time.Time `json:"lastSeenAt"`

	Metrics   *metrics                  `json:"metrics"`
	Errors    []collectionError         `json:"errors"`
	Snapshots []metricSnapshot          `json:"snapshots"`
	Views     []string                  `json:"views"`
	Attention map[string]classification `json:"attention"`
}

// metrics are the collected measurements. Every field here is rendered
// somewhere — candidate metadata, an expansion, or both. The catalog carries
// more; a field nobody renders would be weight on the wire with nothing on the
// other end reading it.
type metrics struct {
	HeadSha              string     `json:"headSha"`
	Branch               string     `json:"branch"`
	LatestCommitAt       *time.Time `json:"latestCommitAt"`
	LatestCommitSubject  string     `json:"latestCommitSubject"`
	LatestCommitShortSha string     `json:"latestCommitShortSha"`
	CommitCount          *int       `json:"commitCount"`
	Commits7d            *int       `json:"commits7d"`
	Commits30d           *int       `json:"commits30d"`
	Commits90d           *int       `json:"commits90d"`
	ActiveDays30d        *int       `json:"activeDays30d"`
	ContributorCount     *int       `json:"contributorCount"`
	DirtyFiles           *int       `json:"dirtyFiles"`
	AheadCount           *int       `json:"aheadCount"`
	BehindCount          *int       `json:"behindCount"`
	LatestTag            string     `json:"latestTag"`

	LocCode          *int   `json:"locCode"`
	LocTest          *int   `json:"locTest"`
	LocFiles         *int   `json:"locFiles"`
	DominantLanguage string `json:"dominantLanguage"`

	TdOpenCount           *int `json:"tdOpenCount"`
	TdInProgressCount     *int `json:"tdInProgressCount"`
	TdBlockedCount        *int `json:"tdBlockedCount"`
	TdReviewCount         *int `json:"tdReviewCount"`
	TdTotalNonClosedCount *int `json:"tdTotalNonClosedCount"`
	TdStaleCount          *int `json:"tdStaleCount"`

	GithubOwner              string     `json:"githubOwner"`
	GithubName               string     `json:"githubName"`
	GithubVisibility         string     `json:"githubVisibility"`
	GithubIsArchived         *bool      `json:"githubIsArchived"`
	GithubStars              *int       `json:"githubStars"`
	GithubForks              *int       `json:"githubForks"`
	GithubOpenIssues         *int       `json:"githubOpenIssues"`
	GithubOpenPrs            *int       `json:"githubOpenPrs"`
	GithubExternalPrs        *int       `json:"githubExternalPrs"`
	GithubOldestExternalPrAt *time.Time `json:"githubOldestExternalPrAt"`
	GithubMergedPrs30d       *int       `json:"githubMergedPrs30d"`
	GithubExternalIssues30d  *int       `json:"githubExternalIssues30d"`
	GithubLatestReleaseAt    *time.Time `json:"githubLatestReleaseAt"`
	GithubLatestReleaseTag   string     `json:"githubLatestReleaseTag"`
	GithubCiState            string     `json:"githubCiState"`
	GithubAvailability       string     `json:"githubAvailability"`

	GithubTrafficViews          *int   `json:"githubTrafficViews"`
	GithubTrafficUniqueVisitors *int   `json:"githubTrafficUniqueVisitors"`
	GithubTrafficClones         *int   `json:"githubTrafficClones"`
	GithubTrafficAvailability   string `json:"githubTrafficAvailability"`

	GitScannedAt    *time.Time `json:"gitScannedAt"`
	LocScannedAt    *time.Time `json:"locScannedAt"`
	TdScannedAt     *time.Time `json:"tdScannedAt"`
	GithubScannedAt *time.Time `json:"githubScannedAt"`
}

// collectionError is one collector's unresolved warning about a project. It is
// data, not a Recall-level failure: ongoing surfaces it as an attention reason,
// and the collector that failed is named so a reader knows which measurements
// are missing.
type collectionError struct {
	Collector  string    `json:"collector"`
	Message    string    `json:"message"`
	OccurredAt time.Time `json:"occurredAt"`
}

// metricSnapshot is one daily value of one tracked metric. The catalog keeps a
// short window of them, which is the only history it has and the whole of what
// a `context` expansion can honestly show.
type metricSnapshot struct {
	Metric     string  `json:"metric"`
	CapturedOn string  `json:"capturedOn"`
	Value      float64 `json:"value"`
}

// classification is one attention view's verdict for one project. The payload
// repeats the view's key inside it; the map key is the same fact, so only the
// verdict and its reasons are read.
type classification struct {
	Member  bool     `json:"member"`
	Reasons []reason `json:"reasons"`
}

// reason is why a classification held. All four of input, value, comparison,
// and threshold travel together: a message alone is a claim, and these are the
// evidence behind it.
type reason struct {
	Source     string `json:"source"`
	Message    string `json:"message"`
	Input      string `json:"input"`
	Value      any    `json:"value"`
	Comparison string `json:"comparison"`
	Threshold  any    `json:"threshold"`
}

// parseCatalog decodes GET /api/projects.
//
// A page model that names its own load error is a source that did not answer,
// not a catalog with no projects — the distinction invariant 2 exists to hold.
func parseCatalog(body []byte) (*catalog, error) {
	var cat catalog
	if err := json.Unmarshal(body, &cat); err != nil {
		return nil, protocol.Errorf(protocol.CodeSourceUnavailable,
			"the catalog response is not the expected shape: %v", err)
	}
	if cat.LoadError != nil && strings.TrimSpace(*cat.LoadError) != "" {
		return nil, protocol.Errorf(protocol.CodeSourceUnavailable,
			"the catalog reported a load error")
	}
	if cat.GeneratedAt.IsZero() {
		// Every freshness statement this adapter makes is measured against
		// generatedAt. Without it there is no honest freshness answer, and a
		// silent fallback to this machine's clock would be one.
		return nil, protocol.Errorf(protocol.CodeSourceUnavailable,
			"the catalog response carries no generatedAt, so its age cannot be established")
	}
	return &cat, nil
}

// boundary is when the last complete successful scan finished, and whether
// there was one. It is what [recall.Candidate.ConfirmedAt] and
// [recall.Health.LastSuccess] mean for this source.
func (c *catalog) boundary() (time.Time, bool) {
	if c.Scan == nil || c.Scan.Status != scanCompleted || c.Scan.FinishedAt == nil {
		return time.Time{}, false
	}
	return c.Scan.FinishedAt.UTC(), true
}

// age is how long before this read the last complete scan finished.
//
// Both ends come from the source: generatedAt is the ongoing server's clock at
// the moment it built this response, and finishedAt is its clock when the scan
// ended. Measuring against the local clock instead would report a skewed host
// as a stale catalog, and would make a recorded response answer differently
// every day it is replayed.
func (c *catalog) age() (time.Duration, bool) {
	finished, ok := c.boundary()
	if !ok {
		return 0, false
	}
	return c.GeneratedAt.UTC().Sub(finished), true
}

func (c *catalog) stale() bool {
	age, ok := c.age()
	return ok && age > StaleAfter
}

// coverage reports whether the catalog represents a complete pass over the
// scan roots.
//
// A scan that is still running, or that failed, leaves the catalog holding
// whatever the previous pass left plus whatever this one has written so far. A
// project absent from it is unknown, not gone, which is invariant 5, so the
// boundary is reported partial rather than complete.
func (c *catalog) coverage() recall.IndexCoverage {
	switch {
	case c.Scan == nil:
		return recall.IndexUnknown
	case c.Scan.Status != scanCompleted:
		return recall.IndexPartial
	default:
		return recall.IndexComplete
	}
}

// watermark is freshness evidence a caller can compare between two searches.
// The scan run id changes with every pass, so two searches quoting the same
// watermark read the same catalog revision.
func (c *catalog) watermark() string {
	if c.Scan == nil {
		return fmt.Sprintf("scan=none projects=%d", len(c.Projects))
	}
	finished := "-"
	if c.Scan.FinishedAt != nil {
		finished = c.Scan.FinishedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("scan=%s status=%s finished=%s projects=%d",
		c.Scan.ID, c.Scan.Status, finished, len(c.Projects))
}

// revision identifies the catalog state a candidate was read from. It is the
// scan run id, because that is the source's own name for one pass over the
// catalog and it does not change until the next pass replaces it.
func (c *catalog) revision() string {
	if c.Scan == nil {
		return "unscanned"
	}
	return c.Scan.ID
}

// warnings counts the projects carrying an unresolved collector warning.
//
// This is not partial coverage and does not degrade health: ongoing surfaces
// these as an attention reason, so a warning is the catalog working — it
// measured what it could and said which collector failed. Reporting it as a
// Recall coverage failure would make every honest catalog look broken.
func (c *catalog) warnings() int {
	n := 0
	for _, p := range c.Projects {
		if len(p.Errors) > 0 {
			n++
		}
	}
	return n
}

// viewCounts is how many projects each classification currently holds,
// recomputed from the projects this source can see rather than read from the
// payload's own counts, which are taken before hidden projects are removed.
func (c *catalog) viewCounts() map[string]any {
	counts := map[string]any{}
	for _, key := range viewKeys {
		n := 0
		for i := range c.Projects {
			if c.Projects[i].inView(key) {
				n++
			}
		}
		if n > 0 {
			counts[key] = n
		}
	}
	return counts
}

func (c *catalog) find(id string) (*project, bool) {
	for i := range c.Projects {
		if c.Projects[i].ID == id {
			return &c.Projects[i], true
		}
	}
	return nil, false
}

// memberViews returns the classifications this project belongs to, in
// ongoing's own order. The payload's `views` array is authoritative; the
// per-view `member` flag is used as a fallback so a payload carrying only one
// of the two still classifies.
func (p *project) memberViews() []string {
	out := make([]string, 0, len(viewKeys))
	for _, key := range viewKeys {
		if p.inView(key) {
			out = append(out, key)
		}
	}
	return out
}

func (p *project) inView(key string) bool {
	for _, v := range p.Views {
		if v == key {
			return true
		}
	}
	if c, ok := p.Attention[key]; ok {
		return c.Member
	}
	return false
}

// reasonsFor returns one view's recorded reasons, or nothing when the project
// is not a member of it.
func (p *project) reasonsFor(key string) []reason {
	if !p.inView(key) {
		return nil
	}
	return p.Attention[key].Reasons
}

// eventTime is when this project last did something: its latest commit. A
// project that has never been scanned for git, or has no commits, has no event
// time rather than a guessed one.
func (p *project) eventTime() *time.Time {
	if p.Metrics == nil || p.Metrics.LatestCommitAt == nil {
		return nil
	}
	at := p.Metrics.LatestCommitAt.UTC()
	return &at
}

// snapshotSeries returns one tracked metric's daily values, oldest first. The
// catalog stores them unordered by day within a metric, so the order is
// established here and not assumed.
func (p *project) snapshotSeries(metric string) []metricSnapshot {
	var out []metricSnapshot
	for _, s := range p.Snapshots {
		if s.Metric == metric {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CapturedOn < out[j].CapturedOn })
	return out
}

// staleCollectors counts, per collector, how many projects carry a measurement
// older than ongoing's own freshness window — or none at all.
//
// The catalog-level age check is not enough on its own. ongoing applies the
// 72-hour rule PER COLLECTOR (attention.ts checks gitScannedAt, locScannedAt,
// tdScannedAt, githubScannedAt and the traffic stamp individually), and a
// classification whose inputs are stale is simply not computed — so the server
// returns membership false, and a source reporting "healthy, coverage complete"
// turns that silence into "nothing qualifies". A recent scan that refreshed git
// while leaving LOC four days old is exactly that case, and it is the normal
// state of this deployment rather than an edge case.
//
// Measured against generatedAt, never the local clock, for the same reason age
// is: a recorded response must replay identically forever.
func (c *catalog) staleCollectors() map[string]any {
	type counter struct{ stale, missing int }
	counters := map[string]*counter{
		"git": {}, "loc": {}, "td": {}, "github": {},
	}
	at := c.GeneratedAt.UTC()
	for i := range c.Projects {
		p := &c.Projects[i]
		if p.Metrics == nil {
			for name := range counters {
				counters[name].missing++
			}
			continue
		}
		for name, stamp := range map[string]*time.Time{
			"git":    p.Metrics.GitScannedAt,
			"loc":    p.Metrics.LocScannedAt,
			"td":     p.Metrics.TdScannedAt,
			"github": p.Metrics.GithubScannedAt,
		} {
			switch {
			case stamp == nil:
				counters[name].missing++
			case at.Sub(stamp.UTC()) > StaleAfter:
				counters[name].stale++
			}
		}
	}
	out := map[string]any{}
	for name, c := range counters {
		if c.stale > 0 {
			out[name+"_stale"] = c.stale
		}
		if c.missing > 0 {
			out[name+"_unmeasured"] = c.missing
		}
	}
	return out
}

// collectorsDegraded reports whether any collector holds an EXPIRED
// measurement.
//
// Absence is not degradation, and conflating the two would make this useless:
// most projects have no td workspace and many have no GitHub remote, so an
// unmeasured collector is the normal, correct state for them and says nothing
// about freshness. An expired one is different — the measurement was taken,
// ongoing's rule now refuses to use it, and a classification silently stops
// being computed. Only that flips health; the unmeasured counts are reported
// beside it so a reader can tell the two apart.
func (c *catalog) collectorsDegraded() bool {
	for k, v := range c.staleCollectors() {
		if n, ok := v.(int); ok && n > 0 && strings.HasSuffix(k, "_stale") {
			return true
		}
	}
	return false
}

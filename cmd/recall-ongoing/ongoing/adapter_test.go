package ongoing_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/cmd/recall-ongoing/ongoing"
	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// location is the endpoint every fixture-backed test claims to describe.
// Nothing contacts it: a `replay` setting answers from disk instead.
const location = "http://ongoing.example:7766"

// catalogFixture is a small ongoing catalog: one project with momentum, one
// carrying three classifications and every reason behind them, and one that has
// never been scanned. generatedAt is eight hours after the scan finished, so
// the catalog is fresh by ongoing's own 72-hour rule — and stays fresh forever,
// because both ends of that arithmetic are stated here rather than read from a
// clock.
const catalogFixture = `{
  "generatedAt": "2026-07-24T12:00:00.000Z",
  "hiddenCount": 1,
  "totalCount": 4,
  "loadError": null,
  "scan": {
    "id": "scan_aaaa", "reason": "cli", "status": "completed",
    "startedAt": "2026-07-24T04:00:00.000Z", "finishedAt": "2026-07-24T04:05:00.000Z",
    "discoveredCount": 3, "updatedCount": 3, "errorCount": 0
  },
  "projects": [
    {
      "id": "project_recall", "name": "recall", "relativePath": "recall",
      "canonicalPath": "/srv/code/recall", "isFavorite": true, "isMissing": false,
      "note": "Agent memory", "intent": "invest", "nextAction": "Ship the ongoing adapter",
      "lastSeenAt": "2026-07-24T04:01:00.000Z",
      "metrics": {
        "branch": "main", "latestCommitAt": "2026-07-24T03:12:00.000Z",
        "latestCommitSubject": "feat(eval): the first baseline",
        "latestCommitShortSha": "0badc0d",
        "commits30d": 24, "activeDays30d": 6, "locCode": 31240,
        "dominantLanguage": "Go", "tdOpenCount": 21, "tdBlockedCount": 0, "tdStaleCount": 0,
        "gitScannedAt": "2026-07-24T04:01:00.000Z"
      },
      "snapshots": [
        {"metric": "loc_code", "capturedOn": "2026-07-20", "value": 24100},
        {"metric": "loc_code", "capturedOn": "2026-07-24", "value": 31240}
      ],
      "errors": [],
      "views": ["momentum"],
      "attention": {
        "momentum": {"key": "momentum", "member": true, "reasons": [
          {"source": "git", "message": "24 commits in 30 days", "input": "commits30d",
           "value": 24, "comparison": ">=", "threshold": 10}
        ]}
      }
    },
    {
      "id": "project_hnbooks", "name": "hnbooks", "relativePath": "hnbooks",
      "canonicalPath": "/srv/code/hnbooks", "isFavorite": false, "isMissing": false,
      "note": "", "intent": null, "nextAction": null,
      "lastSeenAt": "2026-07-24T04:02:00.000Z",
      "metrics": {
        "branch": "main", "latestCommitAt": "2026-02-10T09:30:00.000Z",
        "latestCommitSubject": "chore: bump the scraper user agent",
        "commits30d": 0, "activeDays30d": 0, "locCode": 4200,
        "dominantLanguage": "Python",
        "githubOwner": "marcus", "githubName": "hnbooks", "githubStars": 137,
        "githubExternalPrs": 1, "githubOpenIssues": 3, "githubCiState": "success",
        "gitScannedAt": "2026-07-24T04:02:00.000Z",
        "githubScannedAt": "2026-07-24T04:02:00.000Z"
      },
      "snapshots": [],
      "errors": [],
      "views": ["attention", "opportunity", "dormant"],
      "attention": {
        "attention": {"key": "attention", "member": true, "reasons": [
          {"source": "github", "message": "Oldest external PR is 114 days old",
           "input": "githubOldestExternalPrAgeDays", "value": 114, "comparison": ">=", "threshold": 30}
        ]},
        "opportunity": {"key": "opportunity", "member": true, "reasons": [
          {"source": "git", "message": "Only 0 commits in 30 days", "input": "commits30d",
           "value": 0, "comparison": "<=", "threshold": 5},
          {"source": "github", "message": "137 stars show established interest",
           "input": "githubStars", "value": 137, "comparison": ">=", "threshold": 100},
          {"source": "github", "message": "1 external PR(s) are open",
           "input": "githubExternalPrs", "value": 1, "comparison": ">", "threshold": 0}
        ]},
        "dormant": {"key": "dormant", "member": true, "reasons": [
          {"source": "git", "message": "No commits in 30 days; latest commit is 164 days old",
           "input": "latestCommitAgeDays", "value": 164, "comparison": ">=", "threshold": 90},
          {"source": "decision", "message": "Project is not marked invest",
           "input": "intent", "value": null, "comparison": "!=", "threshold": "invest"}
        ]}
      }
    },
    {
      "id": "project_atlas", "name": "atlas", "relativePath": "atlas",
      "canonicalPath": "/srv/code/atlas", "isFavorite": false, "isMissing": false,
      "note": "", "intent": null, "nextAction": null,
      "lastSeenAt": "2026-07-24T04:04:00.000Z",
      "metrics": null, "snapshots": [], "errors": [], "views": [], "attention": {}
    }
  ]
}`

// fixture writes a recorded catalog and returns the directory holding it.
func fixture(t *testing.T, catalog string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "projects.200.json"), catalog)
	write(t, filepath.Join(dir, "health.200.json"), `{"ok":true}`)
	return dir
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// start hands the adapter a handshake and returns it ready to answer.
func start(t *testing.T, settings map[string]any) *ongoing.Adapter {
	t.Helper()
	a, manifest, err := handshake(t, settings)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if manifest.ProtocolVersion != protocol.MaxVersion {
		t.Fatalf("negotiated version %d, want %d", manifest.ProtocolVersion, protocol.MaxVersion)
	}
	return a
}

func handshake(t *testing.T, settings map[string]any) (*ongoing.Adapter, recall.Manifest, error) {
	t.Helper()
	a := ongoing.New(ongoing.Options{Clock: fixedClock})
	manifest, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            t.TempDir(),
		SourceID:           "ongoing",
		Location:           location,
		Settings:           settings,
	})
	t.Cleanup(func() { _ = a.Close() })
	return a, manifest, err
}

// replaying builds a settings block pointed at a recording of the given
// catalog. Every fixture-backed test goes through the same `replay` code path a
// conformance transcript uses, so nothing here exercises a seam the shipped
// binary does not have.
func replaying(t *testing.T, catalog string, extra map[string]any) map[string]any {
	t.Helper()
	settings := map[string]any{"replay": fixture(t, catalog)}
	for k, v := range extra {
		settings[k] = v
	}
	return settings
}

// fixedClock pins observation time so a test can assert that the adapter reads
// its clock rather than inventing a timestamp from the payload.
var clockAt = time.Date(2030, 3, 4, 5, 6, 7, 0, time.UTC)

func fixedClock() time.Time { return clockAt }

func soon() time.Time { return time.Now().Add(time.Minute) }

func TestManifestDeclaresWhatThisSourceActuallyIs(t *testing.T) {
	// Every field here is a promise the core validates a configuration
	// against, and three of them are the whole shape of this adapter: a
	// project catalog, read live, with no history to filter on.
	_, manifest, err := handshake(t, replaying(t, catalogFixture, nil))
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if len(manifest.RecordTypes) != 1 || manifest.RecordTypes[0] != ongoing.RecordProject {
		t.Errorf("record types = %v, want [%s]", manifest.RecordTypes, ongoing.RecordProject)
	}
	if !manifest.Supports(recall.FreshnessLive) || manifest.Supports(recall.FreshnessIndexed) {
		t.Errorf("freshness modes = %v, want live only", manifest.FreshnessModes)
	}
	if manifest.AsOfSupport != recall.AsOfNone {
		t.Errorf("as_of_support = %q; the catalog stores no history to filter on", manifest.AsOfSupport)
	}
	if manifest.Can(recall.CapCheckpoint) {
		t.Error("checkpoint is declared, but this source owns no projection to check point of")
	}
	if !manifest.Can(recall.CapSearch) || !manifest.Can(recall.CapExpand) {
		t.Errorf("capabilities = %v, want search and expand", manifest.Capabilities)
	}
	if manifest.SettingsSchema == nil {
		t.Error("no settings schema; recall doctor validates configuration against it")
	}
}

func TestTheProjectRecordTypeSurvivesTheWireSchema(t *testing.T) {
	// The protocol declares the record type set open, and this adapter takes
	// it at its word. If a schema ever closed the set, every candidate this
	// source produces would fail validation inside the server rather than
	// somewhere a person would look — so the manifest is checked against the
	// real schema here, not against a copy of the enum.
	schemas, err := protocol.Schemas()
	if err != nil {
		t.Fatalf("load schemas: %v", err)
	}
	_, manifest, err := handshake(t, replaying(t, catalogFixture, nil))
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := schemas.ValidateResult(protocol.MethodInitialize, encoded); err != nil {
		t.Fatalf("the core rejects record_type %q without a core change: %v",
			ongoing.RecordProject, err)
	}
}

func TestAVersionRangeAboveThisBuildFailsTheHandshake(t *testing.T) {
	// Degrading to a version neither end implements is what the handshake
	// exists to prevent, so a range with no overlap is an error and not a
	// quiet downgrade.
	a := ongoing.New(ongoing.Options{})
	t.Cleanup(func() { _ = a.Close() })
	_, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: protocol.MaxVersion + 1,
		ProtocolVersionMax: protocol.MaxVersion + 2,
		Workdir:            t.TempDir(),
		SourceID:           "ongoing",
		Location:           location,
	})
	var verr *protocol.VersionError
	if !errors.As(err, &verr) {
		t.Fatalf("initialize error = %v, want a version error", err)
	}
}

func TestSettingsRefuseAnythingTheAdapterDoesNotRead(t *testing.T) {
	// A misspelled setting that silently did nothing would be configuration
	// with no code path behind it. The case that matters most is the last one:
	// a settings block travels inside a committed configuration file, and this
	// is what stops an access secret being written into one.
	for name, settings := range map[string]map[string]any{
		"unknown key":     {"max_candidate": 3},
		"access secret":   {"access_secret": "hunter2hunter2hunter2"},
		"unknown view":    {"views": []any{"burning"}},
		"negative bound":  {"max_candidates": -1},
		"negative stall":  {"debug_stall_ms": -1},
		"missing replay":  {"replay": "/nonexistent/recording"},
		"relative replay": {"replay": "recording"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := handshake(t, settings)
			if !errors.Is(err, protocol.ErrInvalidParams) {
				t.Fatalf("initialize error = %v, want invalid_params", err)
			}
		})
	}
}

func TestTheLocationMustNameAnInstanceAndNotCarryCredentials(t *testing.T) {
	// Settings and locations are adapter-owned and unvalidated when
	// configuration loads, so this is the layer that has to refuse a file URL
	// and a URL with a password in it. Nothing above it will.
	for name, loc := range map[string]string{
		"empty":       "",
		"not a URL":   "aerie.invalid:7766",
		"file scheme": "file:///etc/passwd",
		"no host":     "http://",
		"credentials": "http://user:secret@ongoing.example:7766",
		"with a path": "http://ongoing.example:7766/api/projects",
	} {
		t.Run(name, func(t *testing.T) {
			a := ongoing.New(ongoing.Options{})
			t.Cleanup(func() { _ = a.Close() })
			_, err := a.Initialize(context.Background(), adapter.Config{
				ProtocolVersionMin: protocol.MinVersion,
				ProtocolVersionMax: protocol.MaxVersion,
				Workdir:            t.TempDir(),
				SourceID:           "ongoing",
				Location:           loc,
			})
			if !errors.Is(err, protocol.ErrInvalidParams) {
				t.Fatalf("initialize error = %v, want invalid_params", err)
			}
		})
	}
}

func TestHealthReportsAFreshCatalogHealthy(t *testing.T) {
	a := start(t, replaying(t, catalogFixture, nil))
	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthHealthy {
		t.Errorf("status = %q, want healthy: %v", health.Status, health.Diagnostics)
	}
	if health.Coverage != recall.IndexComplete {
		t.Errorf("coverage = %q, want complete", health.Coverage)
	}
	if health.RecordCount != 3 {
		t.Errorf("record_count = %d, want the three visible projects", health.RecordCount)
	}
	if health.LastSuccess == nil || !health.LastSuccess.Equal(time.Date(2026, 7, 24, 4, 5, 0, 0, time.UTC)) {
		t.Errorf("last_success_at = %v, want the last completed scan's finish", health.LastSuccess)
	}
	if got := health.Diagnostics["hidden_projects"]; got != 1 {
		t.Errorf("hidden_projects = %v, want the owner's own exclusion reported", got)
	}
	if !strings.Contains(health.SourceWatermark, "scan_aaaa") {
		t.Errorf("watermark = %q, want the scan run that produced it", health.SourceWatermark)
	}
}

func TestStalenessIsMeasuredFromThePayloadAndNotFromTheLocalClock(t *testing.T) {
	// The clock here is set in 2030, years past every timestamp in the
	// fixture. The catalog is still fresh, because freshness is generatedAt
	// minus the scan's finish — both stated by the source. Measuring against
	// the local clock would make a skewed host look like a stale catalog and
	// would make this very transcript answer differently every year.
	a := start(t, replaying(t, catalogFixture, nil))
	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthHealthy {
		t.Fatalf("status = %q with a clock in 2030; freshness must come from the payload",
			health.Status)
	}
	if !health.CheckedAt.Equal(clockAt) {
		t.Errorf("checked_at = %v, want the injected clock: the probe time is local, the age is not",
			health.CheckedAt)
	}
}

func TestAStaleCatalogIsDegradedWithTheRuleItBroke(t *testing.T) {
	// ongoing's own rule is 72 hours: past it, a measurement satisfies no
	// attention classification at all, so a catalog older than that is serving
	// verdicts its own product rules would refuse to compute.
	stale := strings.Replace(catalogFixture,
		`"finishedAt": "2026-07-24T04:05:00.000Z"`, `"finishedAt": "2026-07-18T04:05:00.000Z"`, 1)
	a := start(t, replaying(t, stale, nil))

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthDegraded {
		t.Fatalf("status = %q, want degraded", health.Status)
	}
	if got := health.Diagnostics["catalog_age_hours"]; got != 151 {
		t.Errorf("catalog_age_hours = %v, want 151", got)
	}
	if got := health.Diagnostics["freshness_rule_hours"]; got != 72 {
		t.Errorf("freshness_rule_hours = %v, want ongoing's own 72", got)
	}
	// A stale catalog is behind, not incomplete: it answered for every project
	// it holds, and a search over it still succeeds.
	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "hnbooks", Limit: 5, Deadline: soon(),
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Outcome != recall.SearchSuccess {
		t.Errorf("outcome = %q, want success; a stale catalog is behind, not partial", resp.Outcome)
	}
	if resp.Diagnostics["stale"] != true {
		t.Errorf("diagnostics = %v, want the staleness stated", resp.Diagnostics)
	}
}

func TestAnUnfinishedScanIsPartialAndNeverSilentlyComplete(t *testing.T) {
	// A project absent from a pass that has not finished is unknown, not gone.
	running := strings.Replace(catalogFixture, `"status": "completed"`, `"status": "running"`, 1)
	running = strings.Replace(running, `"finishedAt": "2026-07-24T04:05:00.000Z"`, `"finishedAt": null`, 1)
	a := start(t, replaying(t, running, nil))

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthDegraded || health.Coverage != recall.IndexPartial {
		t.Errorf("health = %s/%s, want degraded/partial", health.Status, health.Coverage)
	}
	resp, err := a.Search(context.Background(), recall.SearchRequest{Limit: 10, Deadline: soon()})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Outcome != recall.SearchPartial {
		t.Errorf("outcome = %q, want partial", resp.Outcome)
	}
	if len(resp.Candidates) == 0 {
		t.Error("a partial pass still answers with what it has")
	}
}

func TestRefreshReportsHealthAndClaimsNoWork(t *testing.T) {
	// This source owns no projection, so recall/refresh has nothing to bring
	// up to date. Returning health unchanged is the honest answer; reporting
	// success for work never done would let a caller believe a stale catalog
	// had been rescanned.
	a := start(t, replaying(t, catalogFixture, nil))
	before, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	after, err := a.Refresh(context.Background(), protocol.RefreshParams{Deadline: soon(), Full: true})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if before.SourceWatermark != after.SourceWatermark || before.Status != after.Status {
		t.Errorf("refresh moved the source: %q/%s became %q/%s",
			before.SourceWatermark, before.Status, after.SourceWatermark, after.Status)
	}
}

func TestUseAfterCloseFailsRatherThanAnswering(t *testing.T) {
	a := start(t, replaying(t, catalogFixture, nil))
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	resp, err := a.Search(context.Background(), recall.SearchRequest{Query: "recall", Deadline: soon()})
	if !errors.Is(err, adapter.ErrClosed) {
		t.Fatalf("search after close = %v, want ErrClosed", err)
	}
	if resp.Outcome == recall.SearchSuccess || len(resp.Candidates) > 0 {
		t.Errorf("a closed adapter answered with outcome %q and %d candidates",
			resp.Outcome, len(resp.Candidates))
	}
}

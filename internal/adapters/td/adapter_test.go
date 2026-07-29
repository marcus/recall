package td_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapters/td"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// The manifest is the basis for eligibility, so every claim in it has to be
// one this adapter can keep.
func TestManifestDeclaresWhatTdCanDo(t *testing.T) {
	a := td.New(td.Options{Runner: recordedWorkspace(t), Clock: fixedClock})
	manifest, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            t.TempDir(),
		SourceID:           "td",
		Location:           workspaceRoot,
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	// as_of is the claim that matters most. td publishes created_at,
	// updated_at, and closed_at, which is more history than the Tasks CLI
	// offers and still not record history: there is no prior revision of a
	// title or a description, and updated_at is a single last-write stamp. An
	// issue edited after a boundary could only be answered from current state,
	// which docs/spec.md forbids outright.
	if manifest.AsOfSupport != recall.AsOfNone {
		t.Errorf("as_of_support = %q, want none: td stores no record history", manifest.AsOfSupport)
	}
	if manifest.AsOfSupport.Honors() {
		t.Error("as_of_support claims it can honor a historical boundary")
	}

	// Live only. The adapter owns no index, so it must not offer to serve one.
	if !manifest.Supports(recall.FreshnessLive) {
		t.Error("manifest does not declare live")
	}
	for _, mode := range []recall.FreshnessMode{recall.FreshnessIndexed, recall.FreshnessHybrid} {
		if manifest.Supports(mode) {
			t.Errorf("manifest declares %q, but this adapter maintains no index", mode)
		}
	}
	if manifest.Can(recall.CapCheckpoint) {
		t.Error("manifest declares checkpoint, but this adapter owns no projection to rebuild")
	}
	for _, cap := range []recall.Capability{recall.CapSearch, recall.CapExpand} {
		if !manifest.Can(cap) {
			t.Errorf("manifest is missing capability %q", cap)
		}
	}
	if !slices.Equal(manifest.RecordTypes, []recall.RecordType{recall.RecordTask}) {
		t.Errorf("record_types = %v, want [task]", manifest.RecordTypes)
	}
	if manifest.Sensitivity != recall.SensitivityInternal {
		t.Errorf("sensitivity floor = %v, want internal", manifest.Sensitivity)
	}
	if manifest.SettingsSchema == nil {
		t.Error("no settings schema: recall doctor cannot validate a configuration without one")
	}
}

// A source instance is a workspace, so a source that names no location names
// no workspace. Failing the handshake is the only honest answer: the adapter
// would otherwise resolve td's database from whatever directory recall was
// started in, which is a different workspace on every invocation.
func TestHandshakeRefusesASourceWithNoWorkspace(t *testing.T) {
	if _, err := initAdapter(t, recordedWorkspace(t), "", nil); err == nil {
		t.Fatal("a source with no location completed the handshake")
	}
}

func TestSettingsAreValidatedAtHandshake(t *testing.T) {
	cases := []struct {
		name     string
		settings map[string]any
		location string
	}{
		{name: "unknown key", settings: map[string]any{"stauts": []any{"open"}}},
		{name: "unknown status", settings: map[string]any{"statuses": []any{"in-progress"}}},
		{name: "unknown type", settings: map[string]any{"types": []any{"story"}}},
		{
			// A workspace name carrying the locator separator would parse back
			// as a different reference, which is the one way a locator can
			// quietly start naming another record.
			name:     "workspace name that would not survive a locator",
			settings: map[string]any{"workspace": "work:api"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := initAdapter(t, recordedWorkspace(t), workspaceRoot, tc.settings); err == nil {
				t.Fatalf("settings %v were accepted", tc.settings)
			}
		})
	}
}

// Health asks td whether this workspace resolves to a readable database, and
// reports the workspace it resolved alongside the one that was configured.
func TestHealthReportsAReadableWorkspace(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, nil)

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthHealthy {
		t.Fatalf("status = %q (%v), want healthy", health.Status, health.Diagnostics)
	}
	if health.RecordCount != 5 {
		t.Errorf("record_count = %d, want the 5 issues td reports", health.RecordCount)
	}
	if health.Coverage != recall.IndexComplete {
		t.Errorf("coverage = %q, want complete", health.Coverage)
	}
	if health.SourceWatermark != "" {
		t.Errorf("watermark %q from a probe that read no listing; td publishes no revision, "+
			"so there is nothing here to fingerprint", health.SourceWatermark)
	}
	if _, said := health.Diagnostics["watermark"]; !said {
		t.Error("no watermark and no diagnostic saying why; an empty field is indistinguishable from a bug")
	}
	if got := health.Diagnostics["workspace"]; got != "tdfix" {
		t.Errorf("diagnostics[workspace] = %v, want the configured workspace name", got)
	}
	if got := health.Diagnostics["td_project"]; got != "tdfix" {
		t.Errorf("diagnostics[td_project] = %v, want the project td resolved", got)
	}
	if health.LastSuccess == nil {
		t.Error("a healthy probe recorded no last success")
	}
}

// The invariant this adapter exists to keep: a workspace that is missing, or
// that was never initialized, is unavailable. If it were reported as a
// successful empty search, fusion downstream would read "the workspace is
// gone" as "there is no such issue".
func TestMissingWorkspaceIsUnavailableAndNeverEmptySuccess(t *testing.T) {
	// Exactly what td writes when it cannot resolve a database: a colorized
	// banner on stdout ahead of the envelope, and exit 1.
	missing := &fakeCLI{reply: func([]string) (td.Result, error) {
		return td.Result{Stdout: fixture(t, "no_database.json"), ExitCode: 1}, nil
	}}
	a := newAdapter(t, missing, nil)

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health returned an error rather than an unhealthy report: %v", err)
	}
	if health.Status != recall.HealthUnavailable {
		t.Errorf("status = %q, want unavailable", health.Status)
	}
	if health.Coverage != recall.IndexUnknown {
		t.Errorf("coverage = %q, want unknown: nothing confirmed what the workspace holds", health.Coverage)
	}
	if health.RecordCount != 0 {
		t.Errorf("record_count = %d for an unreachable workspace", health.RecordCount)
	}

	resp, err := search(t, a, "adapter")
	if err == nil {
		t.Fatal("search over a missing workspace returned no error")
	}
	if resp.Outcome == recall.SearchSuccess {
		t.Fatalf("outcome = %q for a missing workspace; an empty success is indistinguishable from no matches", resp.Outcome)
	}
	if len(resp.Candidates) != 0 {
		t.Errorf("a failed search returned %d candidates", len(resp.Candidates))
	}
	// The diagnostic a person reads must not carry td's terminal colors.
	if detail, _ := resp.Diagnostics["detail"].(string); strings.ContainsRune(detail, 0x1b) {
		t.Errorf("diagnostics[detail] carries an escape sequence: %q", detail)
	}
}

// A project/root mismatch is not a degraded-but-searchable source. Any
// candidate from it would carry a locator for a database the adapter did not
// verify, and the planner treats degraded health as usable.
func TestIdentityMismatchIsUnavailableAndCannotEmitOrExpand(t *testing.T) {
	base := recordedWorkspace(t)
	reply := base.reply
	base.reply = func(args []string) (td.Result, error) {
		if args[0] == "info" {
			return ok([]byte(`{
				"project":"somewhere-else",
				"database":".todos/issues.db",
				"issues":{"total":5,"open":3,"in_progress":1,"closed":1}
			}`)), nil
		}
		return reply(args)
	}
	a := newAdapter(t, base, nil)

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthUnavailable || health.Usable() {
		t.Fatalf("health = %q (%v), want unusable", health.Status, health.Diagnostics)
	}
	if health.Coverage != recall.IndexUnknown {
		t.Errorf("coverage = %q, want unknown", health.Coverage)
	}
	if _, ok := health.Diagnostics[protocol.DiagStoreIdentity]; ok {
		t.Error("an unverified root was published as store identity")
	}

	resp, err := search(t, a, "adapter")
	if err == nil {
		t.Fatal("search across an unverified database succeeded")
	}
	if resp.Outcome == recall.SearchSuccess || len(resp.Candidates) != 0 {
		t.Fatalf("search = outcome %q with %d candidates, want failed and empty",
			resp.Outcome, len(resp.Candidates))
	}
	if base.countCalls("list") != 0 || base.countCalls("search") != 0 {
		t.Error("identity failure was discovered only after corpus reads began")
	}

	if _, err := expand(t, a, "tdfix/"+idAdapter, recall.DetailFull, 0); err == nil {
		t.Fatal("expand accepted a locator before verifying the opened database")
	}
	if base.countCalls("show") != 0 {
		t.Error("expand read a record before verifying the opened database")
	}
}

// When td can report an authoritative root, that report wins over the
// filesystem mirror. This prevents the inverse failure: refusing a sound
// source merely because the mirror has drifted.
func TestAuthoritativeInfoRootBindsLocatorsAndFingerprints(t *testing.T) {
	base := recordedWorkspace(t)
	reply := base.reply
	actualRoot := filepath.Join(t.TempDir(), "api")
	base.reply = func(args []string) (td.Result, error) {
		if args[0] == "info" {
			return ok([]byte(fmt.Sprintf(`{
				"project":"api",
				"database":%q,
				"issues":{"total":5,"open":3,"in_progress":1,"closed":1}
			}`, filepath.Join(actualRoot, ".todos", "issues.db")))), nil
		}
		return reply(args)
	}
	a, err := initAdapter(t, base, filepath.Join(t.TempDir(), "mirror-was-wrong"),
		map[string]any{"workspace": "api"})
	if err != nil {
		t.Fatalf("initialize refused an assertion before td identified its database: %v", err)
	}

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthHealthy {
		t.Fatalf("health = %q (%v), want healthy", health.Status, health.Diagnostics)
	}
	identity, _ := health.Diagnostics[protocol.DiagStoreIdentity].(string)
	if !strings.HasPrefix(identity, "td:") || strings.Contains(identity, actualRoot) {
		t.Errorf("store_identity = %q, want opaque td hash", identity)
	}

	resp, err := search(t, a, "adapter")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Candidates) == 0 {
		t.Fatal("search returned no candidates")
	}
	top := resp.Candidates[0]
	if !strings.HasPrefix(top.Locator.Local, "api/") {
		t.Errorf("locator = %q, want authoritative workspace api", top.Locator.Local)
	}
	if got := top.Metadata["workspace_store"]; got != identity {
		t.Errorf("candidate workspace_store = %v, want %s", got, identity)
	}
}

func TestSearchPinsEveryEvidenceReadAcrossAssociationABA(t *testing.T) {
	first := filepath.Join(t.TempDir(), "api")
	second := filepath.Join(t.TempDir(), "api")
	cli := abaWorkspace(t, first, second)
	a, err := initAdapter(t, cli, first, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}

	resp, err := search(t, a, "adapter")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Outcome != recall.SearchSuccess || len(resp.Candidates) == 0 {
		t.Fatalf("search outcome = %q with %d candidates, want pinned A evidence",
			resp.Outcome, len(resp.Candidates))
	}
	if strings.Contains(resp.Candidates[0].Title, "ABA B") {
		t.Fatalf("search emitted B-store evidence under an A-store locator: %+v", resp.Candidates[0])
	}
	if cli.ordinaryInvocations() != 1 {
		t.Fatalf("%d commands used mutable configured location, want discovery info only",
			cli.ordinaryInvocations())
	}
	for i, root := range cli.pinnedInvocations() {
		if root != first {
			t.Errorf("pinned command %d used %s, want original A store %s", i, root, first)
		}
	}
	if len(cli.pinnedInvocations()) < 2 {
		t.Fatal("pinned info and evidence commands were not both observed")
	}
}

func TestPreparedSearchCarriesHealthWorkspaceAndBoundsSpawns(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, nil)

	resp, err := preparedSearch(t, a, recall.SearchRequest{
		Query: "vertical slice",
		Limit: 20,
	})
	if err != nil {
		t.Fatalf("prepared search: %v", err)
	}
	if resp.Outcome != recall.SearchSuccess {
		t.Fatalf("outcome = %q, want success", resp.Outcome)
	}

	// One ordinary info establishes the workspace while one pinned info
	// verifies --work-dir beside the list and three text probes. Planning and
	// retrieval are one startup wave, without resolving the mutable configured
	// location twice.
	for command, want := range map[string]int{
		"info": 2, "list": 1, "search": 3,
	} {
		if got := cli.countCalls(command); got != want {
			t.Errorf("%s invocations = %d, want %d", command, got, want)
		}
	}
	if got := cli.ordinaryInvocations(); got != 1 {
		t.Errorf("ordinary invocations = %d, want only the planning info probe", got)
	}
	for i, root := range cli.pinnedInvocations() {
		if root != workspaceRoot {
			t.Errorf("pinned command %d used %q, want %q", i, root, workspaceRoot)
		}
	}
}

func TestPreparedSearchActuallyOverlapsHealthAndPinnedRetrieval(t *testing.T) {
	base := recordedWorkspace(t)
	runner := &overlapRunner{
		fakeCLI:         base,
		ordinaryStarted: make(chan struct{}),
		pinnedStarted:   make(chan struct{}),
	}
	a := newAdapter(t, runner, nil)
	req := recall.SearchRequest{
		Query: "vertical slice", Limit: 20, Deadline: time.Now().Add(time.Second),
	}

	resp, err := preparedSearch(t, a, req)
	if err != nil {
		t.Fatalf("prepared search: %v", err)
	}
	if resp.Outcome != recall.SearchSuccess {
		t.Fatalf("outcome = %q, want success", resp.Outcome)
	}
	if base.ordinaryInvocations() != 1 || len(base.pinnedInvocations()) != 5 {
		t.Fatalf("invocations ordinary/pinned = %d/%d, want 1/5",
			base.ordinaryInvocations(), len(base.pinnedInvocations()))
	}
}

type overlapRunner struct {
	*fakeCLI
	ordinaryStarted chan struct{}
	pinnedStarted   chan struct{}
	ordinaryOnce    sync.Once
	pinnedOnce      sync.Once
}

func (r *overlapRunner) Run(ctx context.Context, args ...string) (td.Result, error) {
	r.ordinaryOnce.Do(func() { close(r.ordinaryStarted) })
	select {
	case <-r.pinnedStarted:
	case <-ctx.Done():
		return td.Result{}, ctx.Err()
	}
	return r.fakeCLI.Run(ctx, args...)
}

func (r *overlapRunner) RunPinned(
	ctx context.Context,
	root string,
	args ...string,
) (td.Result, error) {
	r.pinnedOnce.Do(func() { close(r.pinnedStarted) })
	select {
	case <-r.ordinaryStarted:
	case <-ctx.Done():
		return td.Result{}, ctx.Err()
	}
	return r.fakeCLI.RunPinned(ctx, root, args...)
}

func TestPreparedSearchStaysOnHealthWorkspaceAcrossAssociationABA(t *testing.T) {
	first := filepath.Join(t.TempDir(), "api")
	second := filepath.Join(t.TempDir(), "api")
	cli := abaWorkspace(t, first, second)
	a, err := initAdapter(t, cli, first, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}

	resp, err := preparedSearch(t, a, recall.SearchRequest{Query: "adapter", Limit: 20})
	if err != nil {
		t.Fatalf("prepared search: %v", err)
	}
	if len(resp.Candidates) == 0 || strings.Contains(resp.Candidates[0].Title, "ABA B") {
		t.Fatalf("prepared search crossed from admitted store A to B: %+v", resp.Candidates)
	}
	if got := cli.ordinaryInvocations(); got != 1 {
		t.Fatalf("ordinary invocations = %d, want only Health's discovery", got)
	}
	for _, root := range cli.pinnedInvocations() {
		if root != first {
			t.Fatalf("evidence read %q after Health admitted %q", root, first)
		}
	}
}

func TestPreparedSearchDiscardsWrongMirrorAndFallsBackToHealthStore(t *testing.T) {
	mirror := filepath.Join(t.TempDir(), "api")
	authoritative := filepath.Join(t.TempDir(), "api")
	cli := abaWorkspace(t, authoritative, mirror)
	reply := cli.pinnedReply
	cli.pinnedReply = func(root string, args []string) (td.Result, error) {
		res, err := reply(root, args)
		if root == mirror && args[0] != "info" && err == nil {
			res.Stdout = bytes.ReplaceAll(res.Stdout,
				[]byte("Adapter interface"), []byte("WRONG MIRROR"))
		}
		return res, err
	}
	a, err := initAdapter(t, cli, mirror, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}

	resp, err := preparedSearch(t, a, recall.SearchRequest{Query: "adapter", Limit: 20})
	if err != nil {
		t.Fatalf("prepared search: %v", err)
	}
	if len(resp.Candidates) == 0 {
		t.Fatal("fallback returned no authoritative evidence")
	}
	for _, candidate := range resp.Candidates {
		if strings.Contains(candidate.Title, "WRONG MIRROR") {
			t.Fatalf("speculative mirror evidence escaped: %+v", candidate)
		}
	}
	roots := cli.pinnedInvocations()
	if !slices.Contains(roots, mirror) || !slices.Contains(roots, authoritative) {
		t.Fatalf("pinned roots = %v, want discarded mirror and authoritative fallback", roots)
	}
}

func TestPreparedSearchDiscardsEvidenceWhenPinnedVerificationDisagrees(t *testing.T) {
	first := filepath.Join(t.TempDir(), "api")
	second := filepath.Join(t.TempDir(), "api")
	cli := abaWorkspace(t, first, second)
	reply := cli.pinnedReply
	cli.pinnedReply = func(root string, args []string) (td.Result, error) {
		if args[0] == "info" {
			return workspaceInfoAt(second), nil
		}
		return reply(root, args)
	}
	a, err := initAdapter(t, cli, first, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}

	resp, err := preparedSearch(t, a, recall.SearchRequest{Query: "adapter", Limit: 20})
	if err == nil {
		t.Fatal("pinned info identified another store but search succeeded")
	}
	if len(resp.Candidates) != 0 || !resp.Outcome.Degrades() {
		t.Fatalf("mismatched pinned verification admitted evidence: %+v", resp)
	}
	if cli.countCalls("list") == 0 || cli.countCalls("search") == 0 {
		t.Fatal("test did not exercise the concurrent verification-and-retrieval path")
	}
}

func TestPreparedSearchCannotReuseAnotherRequestsHandshake(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)
	req := recall.SearchRequest{Query: "adapter", Limit: 20, Deadline: time.Now().Add(time.Minute)}
	_, preparation, err := a.PrepareSearch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SearchPrepared(context.Background(), req, preparation); err != nil {
		t.Fatalf("first use: %v", err)
	}
	resp, err := a.SearchPrepared(context.Background(), req, preparation)
	if err == nil || len(resp.Candidates) != 0 {
		t.Fatalf("reused preparation returned evidence: response=%+v err=%v", resp, err)
	}
}

func TestPreparedSearchCannotAnswerAnotherRequest(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)
	preparedReq := recall.SearchRequest{
		Query: "adapter", Limit: 20, Deadline: time.Now().Add(time.Minute),
	}
	_, preparation, err := a.PrepareSearch(context.Background(), preparedReq)
	if err != nil {
		t.Fatal(err)
	}

	otherReq := preparedReq
	otherReq.Query = "lineage"
	resp, err := a.SearchPrepared(context.Background(), otherReq, preparation)
	if err == nil || len(resp.Candidates) != 0 || !resp.Outcome.Degrades() {
		t.Fatalf("another request consumed prepared evidence: response=%+v err=%v", resp, err)
	}
	if _, err := a.SearchPrepared(context.Background(), preparedReq, preparation); err != nil {
		t.Fatalf("request mismatch consumed the preparation: %v", err)
	}
}

func TestPreparedSearchCancellationStopsVerificationAndEvidence(t *testing.T) {
	runner := prepareThenWedge{info: fixture(t, "info.json")}
	a := newAdapter(t, runner, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	req := recall.SearchRequest{
		Query: "vertical slice", Limit: 20, Deadline: time.Now().Add(time.Minute),
	}
	_, preparation, err := a.PrepareSearch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.SearchPrepared(ctx, req, preparation)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want request deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("canceled prepared search returned after %s", elapsed)
	}
	if len(resp.Candidates) != 0 || !resp.Outcome.Degrades() {
		t.Fatalf("canceled prepared search returned evidence: %+v", resp)
	}
}

type prepareThenWedge struct {
	info []byte
}

func (r prepareThenWedge) Run(context.Context, ...string) (td.Result, error) {
	return ok(r.info), nil
}

func (prepareThenWedge) RunPinned(ctx context.Context, _ string, _ ...string) (td.Result, error) {
	<-ctx.Done()
	return td.Result{}, ctx.Err()
}

func TestExpandPinsEveryEvidenceReadAcrossAssociationABA(t *testing.T) {
	first := filepath.Join(t.TempDir(), "api")
	second := filepath.Join(t.TempDir(), "api")
	cli := abaWorkspace(t, first, second)
	a, err := initAdapter(t, cli, first, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}

	resp, err := expand(t, a, "api/"+idAdapter, recall.DetailFull, 0)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(resp.Content, "Adapter interface") {
		t.Fatalf("expand did not return A-store evidence: %+v", resp)
	}
	if cli.ordinaryInvocations() != 1 {
		t.Fatalf("%d commands used mutable configured location, want discovery info only",
			cli.ordinaryInvocations())
	}
	for i, root := range cli.pinnedInvocations() {
		if root != first {
			t.Errorf("pinned command %d used %s, want original A store %s", i, root, first)
		}
	}
	if len(cli.pinnedInvocations()) < 2 {
		t.Fatal("pinned info and show commands were not both observed")
	}
}

func TestSearchFailsClosedWhenRunnerCannotPinWorkDir(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, unpinnedOnly{Runner: cli}, nil)

	resp, err := search(t, a, "adapter")
	if err == nil {
		t.Fatal("search used a runner with no --work-dir pin contract")
	}
	if len(resp.Candidates) != 0 || !resp.Outcome.Degrades() {
		t.Fatalf("unpinable search returned evidence: %+v", resp)
	}
	if cli.countCalls("list") != 0 || cli.countCalls("search") != 0 {
		t.Error("evidence commands ran before the runner's pin capability was established")
	}
}

type unpinnedOnly struct{ td.Runner }

func TestDiagnosticsAndEvidenceNeverDiscloseWorkspacePaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private-owner", "api")
	cli := abaWorkspace(t, root, root)
	a, err := initAdapter(t, cli, root, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	searchResp, err := search(t, a, "adapter")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	expandResp, err := expand(t, a, "api/"+idAdapter, recall.DetailFull, 0)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	rendered := fmt.Sprintf("%v\n%v\n%+v", health.Diagnostics, searchResp, expandResp)
	if strings.Contains(rendered, root) || strings.Contains(rendered, filepath.Dir(root)) {
		t.Fatalf("adapter output disclosed absolute workspace path:\n%s", rendered)
	}
	if identity, _ := health.Diagnostics[protocol.DiagStoreIdentity].(string); !strings.HasPrefix(identity, "td:") {
		t.Errorf("store identity %q is not opaque", identity)
	}
}

// abaWorkspace models a mutable configured association that is A for discovery,
// B while evidence commands run, then A for a hypothetical final probe.
// Pinned commands ignore that association and always answer from their explicit
// root. The tests assert no evidence command reaches the ordinary path at all.
func abaWorkspace(t *testing.T, firstRoot, secondRoot string) *fakeCLI {
	t.Helper()
	base := recordedWorkspace(t)
	reply := base.reply
	base.reply = func(args []string) (td.Result, error) {
		if args[0] == "info" {
			return workspaceInfoAt(firstRoot), nil
		}
		// This is the B interval. The second ordinary info call below would
		// report A again, so the old pre/post check accepted this evidence.
		res, err := reply(args)
		if err == nil {
			res.Stdout = bytes.ReplaceAll(res.Stdout,
				[]byte("Adapter interface"), []byte("ABA B database"))
		}
		_ = secondRoot // names the mutable association's B store in the test.
		return res, err
	}
	base.pinnedReply = func(root string, args []string) (td.Result, error) {
		if args[0] == "info" {
			return workspaceInfoAt(root), nil
		}
		return reply(args)
	}
	return base
}

func workspaceInfoAt(root string) td.Result {
	return ok([]byte(fmt.Sprintf(`{
			"project":"api",
			"database":%q,
			"issues":{"total":5,"open":3,"in_progress":1,"closed":1}
		}`, filepath.Join(root, ".todos", "issues.db"))))
}

func TestSameBasenameSeparateDatabasesDoNotShareFingerprints(t *testing.T) {
	// The recorded info says tdfix, so give each fake the live-style absolute
	// database identity that makes its actual root authoritative.
	makeAPI := func(root string) *fakeCLI {
		base := recordedWorkspace(t)
		reply := base.reply
		base.reply = func(args []string) (td.Result, error) {
			if args[0] == "info" {
				return ok([]byte(fmt.Sprintf(`{
					"project":"api",
					"database":%q,
					"issues":{"total":5,"open":3,"in_progress":1,"closed":1}
				}`, filepath.Join(root, ".todos", "issues.db")))), nil
			}
			return reply(args)
		}
		return base
	}
	rootA := filepath.Join(t.TempDir(), "api")
	rootB := filepath.Join(t.TempDir(), "api")
	first, err := initAdapter(t, makeAPI(rootA), rootA, nil)
	if err != nil {
		t.Fatalf("initialize first authoritative store: %v", err)
	}
	second, err := initAdapter(t, makeAPI(rootB), rootB, nil)
	if err != nil {
		t.Fatalf("initialize second authoritative store: %v", err)
	}

	hitA, err := search(t, first, "adapter")
	if err != nil || len(hitA.Candidates) == 0 {
		t.Fatalf("search first: %v (%v)", err, hitA.Diagnostics)
	}
	hitB, err := search(t, second, "adapter")
	if err != nil || len(hitB.Candidates) == 0 {
		t.Fatalf("search second: %v (%v)", err, hitB.Diagnostics)
	}
	if got, want := hitA.Candidates[0].Locator.Local, hitB.Candidates[0].Locator.Local; got != want {
		t.Fatalf("locators differ (%q, %q); test requires the same basename and issue", got, want)
	}
	if got, want := hitA.Candidates[0].ContentFingerprint, hitB.Candidates[0].ContentFingerprint; got == want {
		t.Fatalf("separate stores share content fingerprint %q", got)
	}
}

func TestConfiguredWorkspaceAssertionIsCheckedAgainstTdInfo(t *testing.T) {
	a, err := initAdapter(t, recordedWorkspace(t), workspaceRoot,
		map[string]any{"workspace": "recall"})
	if err != nil {
		t.Fatalf("initialize decided database identity before td ran: %v", err)
	}
	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Usable() || health.Coverage != recall.IndexUnknown {
		t.Fatalf("mismatched assertion health = %q coverage %q (%v)",
			health.Status, health.Coverage, health.Diagnostics)
	}
}

// Health costs one invocation, because the core probes it once per source per
// query before anything is searched. It used to read the workspace listing too
// — 1.6 MB of JSON on the largest workspace here — for a watermark that only a
// health-only surface displays.
func TestHealthReadsInfoAndNotTheWorkspace(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, nil)

	if _, err := a.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	if n := cli.countCalls("info"); n != 1 {
		t.Errorf("%d info invocations, want 1", n)
	}
	if n := cli.countCalls("list"); n != 0 {
		t.Errorf("%d listing invocations from a health probe, want 0: a listing is what a "+
			"search reads, and health runs on the path of every query", n)
	}
}

// A scope too large for one listing is reported as partial coverage without a
// listing being read, because td's own counts are an upper bound on what a
// listing would return. Losing this signal would leave a source enumerating
// part of its scope while claiming a complete boundary.
func TestAScopeTooLargeToListIsPartialCoverage(t *testing.T) {
	// td's counts, with more issues in the workspace than one listing reads.
	huge := &fakeCLI{reply: func(args []string) (td.Result, error) {
		if args[0] == "info" {
			return ok([]byte(`{"project":"tdfix","issues":{"total":6000,"open":6000}}`)), nil
		}
		t.Errorf("unexpected invocation: td %s", strings.Join(args, " "))
		return td.Result{}, nil
	}}
	a := newAdapter(t, huge, nil)

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthDegraded {
		t.Errorf("status = %q (%v), want degraded", health.Status, health.Diagnostics)
	}
	if health.Coverage != recall.IndexPartial {
		t.Errorf("coverage = %q, want partial: a listing would stop before the end of the scope", health.Coverage)
	}
	if _, said := health.Diagnostics["listing"]; !said {
		t.Error("partial coverage with nothing saying why")
	}
}

// A source scoped to a status is bounded by that status's count and not by the
// whole workspace, so a big archive of closed work does not make an instance
// reading open issues report partial coverage it does not have.
func TestScopedCoverageIsBoundedByTheConfiguredStatuses(t *testing.T) {
	mostlyClosed := &fakeCLI{reply: func(args []string) (td.Result, error) {
		if args[0] == "info" {
			return ok([]byte(`{"project":"tdfix","issues":{"total":6000,"open":40,"closed":5960}}`)), nil
		}
		t.Errorf("unexpected invocation: td %s", strings.Join(args, " "))
		return td.Result{}, nil
	}}
	a := newAdapter(t, mostlyClosed, map[string]any{"statuses": []string{"open"}})

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Coverage != recall.IndexComplete {
		t.Errorf("coverage = %q (%v), want complete: 40 open issues fit in one listing",
			health.Coverage, health.Diagnostics)
	}
}

// Refresh exists because the contract has it. This source owns no projection,
// so it must report health unchanged rather than claim work it never did.
func TestRefreshReportsHealthWithoutClaimingWork(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)

	refreshed, err := a.Refresh(context.Background(), protocol.RefreshParams{})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if refreshed.Status != health.Status || refreshed.SourceWatermark != health.SourceWatermark {
		t.Errorf("refresh reported %q/%q, health reported %q/%q; a live source has nothing to rebuild",
			refreshed.Status, refreshed.SourceWatermark, health.Status, health.SourceWatermark)
	}
	if refreshed.IndexGeneration != "" {
		t.Errorf("refresh published index generation %q for a source with no index", refreshed.IndexGeneration)
	}
}

// A closed adapter must fail rather than answer. An empty result from a closed
// source would be a claim about the workspace that nothing looked at.
func TestClosedAdapterFailsRatherThanAnswers(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	resp, err := search(t, a, "adapter")
	if !errors.Is(err, adapter.ErrClosed) {
		t.Errorf("search after close: err = %v, want ErrClosed", err)
	}
	if resp.Outcome == recall.SearchSuccess {
		t.Errorf("outcome = %q after close", resp.Outcome)
	}
	if _, err := a.Expand(context.Background(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "td", Local: "tdfix/" + idAdapter},
	}); !errors.Is(err, adapter.ErrClosed) {
		t.Errorf("expand after close: err = %v, want ErrClosed", err)
	}
}

// Recorded td output is a supported way to configure this source, because a
// committed evaluation pack cannot spawn td against a workspace that changes
// with every commit.
func TestReplayAnswersWithoutSpawningTd(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/"+td.ReplayFile, `{
      "invocations": [
        {"contains": ["info"], "stdout": "info.json"},
        {"contains": ["list"], "stdout": "list_all.json"},
        {"contains": ["search"], "stdout": "search_adapter.json"}
      ],
      "default": {"stdout": "show_not_found.json", "exit_code": 1}
    }`)
	for _, name := range []string{"info.json", "list_all.json", "search_adapter.json", "show_not_found.json"} {
		writeFile(t, dir+"/"+name, string(fixture(t, name)))
	}

	// No Runner is injected: the replay directory is what makes this adapter
	// answer, which is the whole point of it being configuration.
	a := td.New(td.Options{Clock: fixedClock})
	if _, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            t.TempDir(),
		SourceID:           "td",
		Location:           dir,
		Settings:           map[string]any{"replay": "."},
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	resp, err := search(t, a, "adapter")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := ids(resp); len(got) == 0 || got[0] != idAdapter {
		t.Errorf("replayed search = %v, want %s first", got, idAdapter)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

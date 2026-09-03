package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/pkg/buildinfo"
	"github.com/marcus/recall/pkg/recall"
)

// `recall sidecar-plugin` is Recall as a Sidecar plugin: one JSON request on
// stdin, one JSON response on stdout, one process per call.
//
// Boundary: it is a transport, like `serve` and `mcp`. Every retrieval decision
// is still made in internal/app, every eligibility decision in internal/source.
// What is written here is the projection from Recall's vocabulary into
// Sidecar's — outcome, coverage, suppression, and excerpt kind into collections,
// rows, notices, and documents — and nothing else. It never shells out to
// recall; it is recall.
//
// Two rules from the protocol shape everything below:
//
//   - A typed success and a typed failure BOTH exit 0. Recall's own exit codes
//     say what a query claims about the corpus; here that claim travels in the
//     page's outcome, and a non-zero exit would be read by the host as a
//     transport failure — the plugin crashed — which is a different and false
//     statement. The only non-zero exit is a response that could not be written.
//   - Standard output carries exactly one JSON value and nothing else.
//     Diagnostics go to standard error, which the host drains and discards.
const sidecarProtocol = "sidecar.plugin/v1-draft"

// Typed error codes, from the resource protocol this one grew out of.
const (
	sidecarCodeNotFound       = "not_found"
	sidecarCodeInvalidConfig  = "invalid_config"
	sidecarCodeInvalidRequest = "invalid_request"
	sidecarCodeUnavailable    = "unavailable"
	sidecarCodeInternal       = "internal"
)

// The protocol's five methods. Named rather than written inline because they
// are protocol vocabulary and read like nothing else in this file.
const (
	sidecarMethodDescribe = "describe"
	sidecarMethodResolve  = "resolve"
	sidecarMethodList     = "list"
	sidecarMethodGet      = "get"
	sidecarMethodAct      = "act"
)

// Collection identifiers. They are persisted in Sidecar's pane state, so they
// are part of the contract and not display strings.
const (
	sidecarResults = "results"
	sidecarSources = "sources"
)

// sidecarStdinLimit bounds the request. The host sends one small object; a
// stream that never ends is not one, and reading it would be the plugin's own
// denial of service.
const sidecarStdinLimit = 1 << 20

const sidecarPluginHelp = `usage: recall sidecar-plugin [flags]

Answer one Sidecar plugin request. A single JSON object is read from standard
input and a single JSON response object is written to standard output; logs and
diagnostics go to standard error. The protocol is ` + sidecarProtocol + `.

This command is not meant to be typed. Sidecar runs it, one process per call,
from a plugins.external entry:

  "plugins": {
    "external": [
      {"id": "recall", "command": ["recall", "sidecar-plugin"],
       "enabled": true, "placements": ["tab", "panes"]}
    ]
  }

flags:
  --profile NAME  profile to resolve; default is the configured one

Both a typed success and a typed failure exit 0, because either is an answer.
A non-zero exit means the response itself could not be written.
`

func runSidecarPlugin(ctx context.Context, env Env, args []string) int {
	fs := newFlagSet("sidecar-plugin")
	profile := fs.String("profile", "", "profile to resolve")
	if ok, code := parse(env, fs, sidecarPluginHelp, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageErr(env, sidecarPluginHelp, errors.New("sidecar-plugin takes no arguments"))
	}

	resp := answerSidecar(ctx, env, *profile)
	// report is the whole of the exit-code policy here: 0 for an answer of
	// either kind, non-zero only when the answer could not be delivered.
	return report(env, emitJSON(env.Stdout, resp))
}

// sidecarRequest is the envelope the host writes. Params is held raw because it
// is typed per method: decoding it as a union would let a field meant for one
// method arrive on another.
type sidecarRequest struct {
	Protocol   string          `json:"protocol"`
	Method     string          `json:"method"`
	Instance   string          `json:"instance"`
	DeadlineMs int64           `json:"deadlineMs"`
	Context    *sidecarContext `json:"context"`
	Params     json.RawMessage `json:"params"`
}

// sidecarContext is what the host sends for the kinds this plugin declared.
// Nothing else is ever sent, and nothing else is read here.
type sidecarContext struct {
	Project *sidecarProject `json:"project"`
}

type sidecarProject struct {
	Root    string `json:"root"`
	WorkDir string `json:"workDir"`
	Name    string `json:"name"`
	Branch  string `json:"branch"`
	HostID  string `json:"hostId"`
}

type sidecarListParams struct {
	Collection string           `json:"collection"`
	Query      string           `json:"query"`
	View       string           `json:"view"`
	Sort       sidecarSortOrder `json:"sort"`
	Cursor     string           `json:"cursor"`
	Limit      int              `json:"limit"`
}

type sidecarSortOrder struct {
	Key string `json:"key"`
	Dir string `json:"dir"`
}

type sidecarGetParams struct {
	Collection string `json:"collection"`
	ID         string `json:"id"`
}

type sidecarActParams struct {
	Action     string            `json:"action"`
	Collection string            `json:"collection"`
	ID         string            `json:"id"`
	Inputs     map[string]string `json:"inputs"`
}

// sidecarResponse is the single object written to stdout. Exactly one of the
// describe block, resource, page, outcome, or error is populated.
type sidecarResponse struct {
	Protocol string `json:"protocol"`

	Plugin      *sidecarInfo        `json:"plugin,omitempty"`
	Context     []string            `json:"context,omitempty"`
	Collections []sidecarCollection `json:"collections,omitempty"`
	Actions     []sidecarAction     `json:"actions,omitempty"`

	Resource *sidecarDocument `json:"resource,omitempty"`
	Page     *sidecarPage     `json:"page,omitempty"`
	Outcome  *sidecarOutcome  `json:"outcome,omitempty"`
	Error    *sidecarError    `json:"error,omitempty"`
}

type sidecarInfo struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type sidecarColumn struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Width     int    `json:"width,omitempty"`
	Align     string `json:"align,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Primary   bool   `json:"primary,omitempty"`
	Secondary bool   `json:"secondary,omitempty"`
}

type sidecarSortKey struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Default string `json:"default,omitempty"`
}

type sidecarRefresh struct {
	EverySeconds int `json:"everySeconds,omitempty"`
}

type sidecarCollection struct {
	ID      string           `json:"id"`
	Title   string           `json:"title"`
	Search  string           `json:"search"`
	Columns []sidecarColumn  `json:"columns"`
	Views   []string         `json:"views"`
	Sort    []sidecarSortKey `json:"sort"`
	Detail  bool             `json:"detail"`
	Refresh sidecarRefresh   `json:"refresh"`
}

type sidecarAction struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	On         string `json:"on"`
	Collection string `json:"collection,omitempty"`
	Mutates    bool   `json:"mutates"`
	Confirm    bool   `json:"confirm"`
}

type sidecarStatus struct {
	Label string `json:"label"`
	Tone  string `json:"tone,omitempty"`
}

type sidecarItem struct {
	ID     string            `json:"id"`
	Cells  map[string]string `json:"cells"`
	Status *sidecarStatus    `json:"status,omitempty"`
}

type sidecarNotice struct {
	Tone string `json:"tone"`
	Text string `json:"text"`
}

type sidecarPage struct {
	Outcome    string          `json:"outcome"`
	Items      []sidecarItem   `json:"items"`
	NextCursor string          `json:"nextCursor,omitempty"`
	Total      int             `json:"total,omitempty"`
	Notices    []sidecarNotice `json:"notices,omitempty"`
}

type sidecarField struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Kind  string `json:"kind,omitempty"`
}

type sidecarBody struct {
	Format string `json:"format"`
	Text   string `json:"text"`
}

type sidecarSection struct {
	Title  string         `json:"title"`
	Body   *sidecarBody   `json:"body,omitempty"`
	Fields []sidecarField `json:"fields,omitempty"`
}

type sidecarDocument struct {
	Identity string           `json:"identity"`
	Title    string           `json:"title"`
	Subtitle string           `json:"subtitle,omitempty"`
	Status   *sidecarStatus   `json:"status,omitempty"`
	Fields   []sidecarField   `json:"fields,omitempty"`
	Sections []sidecarSection `json:"sections,omitempty"`
}

type sidecarOutcome struct {
	Status  string       `json:"status"`
	Message string       `json:"message,omitempty"`
	Refresh []string     `json:"refresh,omitempty"`
	Open    *sidecarOpen `json:"open,omitempty"`
}

type sidecarOpen struct {
	Collection string `json:"collection"`
	ID         string `json:"id"`
}

type sidecarError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	SetupHint string `json:"setupHint,omitempty"`
}

func sidecarFail(code, message string, retryable bool) sidecarResponse {
	return sidecarResponse{
		Protocol: sidecarProtocol,
		Error: &sidecarError{
			Code:      code,
			Message:   sidecarLine(message, 400),
			Retryable: retryable,
		},
	}
}

func sidecarUnconfigured(message, hint string) sidecarResponse {
	resp := sidecarFail(sidecarCodeInvalidConfig, message, false)
	resp.Error.SetupHint = sidecarLine(hint, 200)
	return resp
}

// answerSidecar reads one request and produces one response. It never returns
// an error: every way this can go wrong is a typed failure the host renders.
func answerSidecar(ctx context.Context, env Env, profile string) sidecarResponse {
	body, err := io.ReadAll(io.LimitReader(env.stdin(), sidecarStdinLimit+1))
	if err != nil {
		return sidecarFail(sidecarCodeInvalidRequest, "reading the request: "+err.Error(), false)
	}
	if len(body) > sidecarStdinLimit {
		return sidecarFail(sidecarCodeInvalidRequest,
			fmt.Sprintf("request is larger than %d bytes", sidecarStdinLimit), false)
	}

	var req sidecarRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return sidecarFail(sidecarCodeInvalidRequest,
			"the request is not one JSON object: "+err.Error(), false)
	}
	if req.Protocol != sidecarProtocol {
		// Naming what is supported is the whole point of the code: a host on a
		// newer protocol has to learn which one this build speaks, and a plugin
		// that answered a protocol it does not implement would be worse than
		// one that refused.
		return sidecarFail(sidecarCodeInvalidRequest, fmt.Sprintf(
			"protocol %q is not supported; recall speaks %s", req.Protocol, sidecarProtocol), false)
	}

	// The deadline is advisory but accurate, so it is spent rather than
	// ignored: recall is given the budget minus a reserve, and the context is
	// cut a little later still, so a slow source ends in a typed unavailable
	// this plugin wrote instead of a SIGKILL the host has to explain.
	ctx, cancel, budgetMS := sidecarBudget(ctx, req.DeadlineMs)
	defer cancel()

	switch req.Method {
	case sidecarMethodDescribe:
		return sidecarDescribe(env)
	case sidecarMethodList:
		return sidecarList(ctx, env, profile, budgetMS, req)
	case sidecarMethodGet:
		return sidecarGet(ctx, env, profile, req)
	case sidecarMethodAct:
		return sidecarAct(ctx, env, profile, req)
	case sidecarMethodResolve:
		// Declared matchers are what makes resolve reachable, and this plugin
		// declares none; see sidecarDescribe for why.
		return sidecarFail(sidecarCodeInvalidRequest,
			"recall declares no matchers, so "+sidecarProtocol+" resolve has nothing to resolve", false)
	default:
		return sidecarFail(sidecarCodeInvalidRequest, fmt.Sprintf(
			"unknown method %q; %s has %s, %s, %s, %s, and %s",
			req.Method, sidecarProtocol, sidecarMethodDescribe, sidecarMethodResolve,
			sidecarMethodList, sidecarMethodGet, sidecarMethodAct), false)
	}
}

// sidecarBudget turns the host's deadline into a context deadline and a recall
// latency budget.
//
// The reserve is what pays for fusion, rendering, and writing the response
// after the sources have been asked. Recall's own budget stops asking sources
// at the earlier instant; the context stops everything a little after that, so
// the ordinary case is a typed answer and the pathological one is still bounded.
func sidecarBudget(ctx context.Context, deadlineMS int64) (context.Context, context.CancelFunc, int) {
	if deadlineMS <= 0 {
		return ctx, func() {}, 0
	}
	reserve := deadlineMS / 10
	switch {
	case reserve < 100:
		reserve = 100
	case reserve > 1000:
		reserve = 1000
	}
	if reserve >= deadlineMS {
		reserve = deadlineMS / 2
	}
	budget := deadlineMS - reserve
	if budget < 1 {
		budget = 1
	}
	hard := time.Duration(budget+reserve/2) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, hard)
	return ctx, cancel, int(budget)
}

// sidecarDescribe reports identity, the context kinds read, the collections
// offered, and the actions exposed. It is local and fast: it resolves and
// parses configuration, which is what tells an unconfigured install from a
// working one, and it opens no index, contacts no source, and spawns nothing.
//
// There are deliberately no matchers. Recall's locator is display-form
// "<source_id>:<local>", where the source name is whatever the user configured
// and the local part is adapter-owned. A pattern wide enough to match it would
// also match every URL scheme, every "key: value" pair, and every Go import
// path printed in a terminal, and a matcher that fires on all of those is worse
// than none: the protocol's rule is that a matcher is a stable, unambiguous
// shape, and this one is neither.
func sidecarDescribe(env Env) sidecarResponse {
	if resp, ok := sidecarConfigProblem(env); !ok {
		return resp
	}
	return sidecarResponse{
		Protocol: sidecarProtocol,
		Plugin: &sidecarInfo{
			Kind:    "recall",
			Name:    "Recall",
			Version: buildinfo.Version,
		},
		// Project context narrows a request the way `--scope project=` does.
		// It is declared rather than demanded: recall answers globally, and a
		// surface with no project simply sends nothing.
		Context: []string{"project"},
		Collections: []sidecarCollection{
			{
				ID:     sidecarResults,
				Title:  "Results",
				Search: "required",
				Columns: []sidecarColumn{
					{ID: "rank", Label: "#", Width: 3, Align: "right", Kind: "number"},
					{ID: "title", Label: "Title", Primary: true},
					{ID: "source", Label: "Source", Width: 14},
					{ID: "excerpt", Label: "Excerpt", Secondary: true},
				},
				Views: []string{},
				Sort: []sidecarSortKey{
					{ID: "rank", Label: "Relevance", Default: "asc"},
					{ID: "source", Label: "Source"},
					{ID: "updated", Label: "Updated"},
				},
				Detail:  true,
				Refresh: sidecarRefresh{},
			},
			{
				ID:     sidecarSources,
				Title:  "Sources",
				Search: "none",
				Columns: []sidecarColumn{
					{ID: "name", Label: "Source", Primary: true},
					{ID: "health", Label: "Health", Kind: "status"},
					{ID: "fresh", Label: "Fresh", Kind: "timestamp"},
				},
				Views:  []string{},
				Sort:   []sidecarSortKey{},
				Detail: true,
				// Health and freshness change without recall being asked, and
				// there is no one path to watch for it: an index generation, a
				// live source's reachability, and a checkpoint all move
				// independently. A poll while the collection is visible is the
				// honest mechanism.
				Refresh: sidecarRefresh{EverySeconds: 120},
			},
		},
		Actions: []sidecarAction{
			{
				ID:         "refresh-source",
				Title:      "Refresh source",
				On:         "item",
				Collection: sidecarSources,
				Mutates:    true,
				Confirm:    true,
			},
		},
	}
}

// sidecarConfigProblem reports an installed-but-unconfigured recall as the
// typed failure the protocol asks for, with the one line that fixes it.
func sidecarConfigProblem(env Env) (sidecarResponse, bool) {
	paths := env.Paths
	if paths.ConfigHome == "" {
		resolved, err := config.XDGPaths()
		if err != nil {
			return sidecarUnconfigured("recall cannot resolve its configuration directory: "+err.Error(),
				"Set XDG_CONFIG_HOME or HOME for the Sidecar process"), false
		}
		paths = resolved
	}
	if _, err := os.Stat(paths.ConfigFile()); errors.Is(err, os.ErrNotExist) {
		return sidecarUnconfigured("recall has no configuration at "+paths.ConfigFile(),
			"Run recall init --docs DIR"), false
	}
	if _, err := env.load(); err != nil {
		return sidecarUnconfigured("recall configuration did not load: "+err.Error(),
			"Run recall doctor"), false
	}
	return sidecarResponse{}, true
}

// sidecarProjectScope turns declared project context into a recall scope, and
// refuses a surface bound to another machine.
//
// The refusal is the rule the protocol states for a plugin that only knows this
// machine: a remote-bound surface sends that host's paths, and answering them
// from this machine's index would report one checkout's evidence as another's.
func sidecarProjectScope(reqCtx *sidecarContext) (*recall.Scope, *sidecarResponse) {
	if reqCtx == nil || reqCtx.Project == nil {
		return nil, nil
	}
	project := reqCtx.Project
	if project.HostID != "" {
		resp := sidecarFail(sidecarCodeUnavailable, fmt.Sprintf(
			"this recall runs on the machine Sidecar is running on and cannot answer for host %q",
			project.HostID), false)
		return nil, &resp
	}
	if strings.TrimSpace(project.Name) == "" {
		return nil, nil
	}
	return &recall.Scope{Project: project.Name}, nil
}

func sidecarList(ctx context.Context, env Env, profile string, budgetMS int, req sidecarRequest) sidecarResponse {
	var params sidecarListParams
	if resp, ok := sidecarParams(req.Params, &params); !ok {
		return resp
	}
	switch params.Collection {
	case sidecarResults:
		return sidecarListResults(ctx, env, profile, budgetMS, params, req.Context)
	case sidecarSources:
		return sidecarListSources(ctx, env, profile)
	default:
		return sidecarUnknownCollection(params.Collection)
	}
}

func sidecarUnknownCollection(id string) sidecarResponse {
	return sidecarFail(sidecarCodeInvalidRequest, fmt.Sprintf(
		"no collection named %q; recall declares %s and %s", id, sidecarResults, sidecarSources), false)
}

func sidecarParams(raw json.RawMessage, into any) (sidecarResponse, bool) {
	if len(raw) == 0 {
		return sidecarFail(sidecarCodeInvalidRequest, "the request carries no params", false), false
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return sidecarFail(sidecarCodeInvalidRequest, "params did not decode: "+err.Error(), false), false
	}
	return sidecarResponse{}, true
}

// sidecarListResults runs one query through the configured profile.
func sidecarListResults(ctx context.Context, env Env, profile string, budgetMS int,
	params sidecarListParams, reqCtx *sidecarContext,
) sidecarResponse {
	query := strings.TrimSpace(params.Query)
	if query == "" {
		// The host answers an empty query on a search: required collection
		// without starting a process, so this is the path a caller reaches by
		// hand. The answer is the same one and it is abstained rather than
		// answered: recall does not list, it answers, and an empty list from a
		// query nobody made is not a claim about the corpus.
		return sidecarResponse{
			Protocol: sidecarProtocol,
			Page:     &sidecarPage{Outcome: "abstained", Items: []sidecarItem{}},
		}
	}

	scope, refusal := sidecarProjectScope(reqCtx)
	if refusal != nil {
		return *refusal
	}

	// A limit is not negative, and recall refuses one rather than guessing. The
	// host clamps before it sends, so a negative here is a caller by hand: read
	// it as "no limit of your own", which is what zero already means.
	limit := params.Limit
	if limit < 0 {
		limit = 0
	}

	core, closeCore, err := openCore(env, profile, limit, remoteFlags{})
	if err != nil {
		return sidecarUnconfigured("recall could not open its configuration: "+err.Error(),
			"Run recall doctor")
	}
	defer func() { _ = closeCore() }()

	resp, err := core.Query(ctx, recall.QueryRequest{
		Query:   query,
		Profile: profile,
		Scope:   scope,
		Mode:    recall.ModeExplicit,
		Budget: recall.Budget{
			LatencyMS:      budgetMS,
			ResponseTokens: recall.DefaultResponseTokens,
			// The host draws a bounded table of pointers, which is what the
			// pointer projection costs. Pricing this call as the whole
			// serialization would answer Sidecar with fewer results than
			// `recall query --json` answers a script.
			Surface: recall.SurfaceStructuredPointer,
		},
		Limit: limit,
	})
	if err != nil {
		// A request that could not be planned or fused made no claim about the
		// corpus, so it is an error rather than an empty page.
		return sidecarFail(sidecarCodeUnavailable, "recall could not run the query: "+err.Error(), true)
	}
	if resp.Outcome == recall.OutcomeFailed {
		// Every source that was asked failed. The draft protocol's page
		// outcomes are answered, abstained, and degraded, none of which can
		// carry "nothing looked hard enough for an empty list to mean
		// anything", so this is a typed failure instead. It is retryable
		// because the sources, not the request, are what went wrong.
		return sidecarFail(sidecarCodeUnavailable, sidecarFailedMessage(resp), true)
	}

	rows := make([]sidecarRow, 0, len(resp.Results))
	for i, r := range resp.Results {
		rows = append(rows, sidecarRow{rank: i + 1, result: r})
	}
	sidecarSortRows(rows, params.Sort)

	items := make([]sidecarItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, sidecarResultItem(row))
	}

	page := &sidecarPage{
		Outcome: sidecarOutcomeOf(resp),
		Items:   items,
		// Recall answers once, bounded by the profile's max_results and the
		// response budget: there is no second page to point at, and a cursor
		// that always came back empty is the honest report of that.
		NextCursor: "",
		Total:      len(items) + resp.DroppedResults,
		Notices:    sidecarNotices(resp),
	}
	return sidecarResponse{Protocol: sidecarProtocol, Page: page}
}

// sidecarRow is one result with the rank it earned, kept separately so a
// re-sort by another key does not renumber relevance.
type sidecarRow struct {
	rank   int
	result recall.Result
}

// sidecarSortRows applies the host's chosen sort key.
//
// Recall has no sort parameter: fusion order IS the answer, and every other
// key here is applied after the fact to the one page recall returned. That is
// exact rather than approximate only because there is exactly one page; a
// paged source would need the protocol to say which is which.
func sidecarSortRows(rows []sidecarRow, order sidecarSortOrder) {
	key := order.Key
	if key == "" {
		key = "rank"
	}
	desc := order.Dir == "desc"
	if order.Dir == "" && key == "updated" {
		// Newest first is what a reader means by "sort by updated"; the other
		// two keys read the other way.
		desc = true
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if key == "updated" {
			ta, tb := sidecarUpdated(a.result.Primary), sidecarUpdated(b.result.Primary)
			// A record whose source said nothing about time sorts last
			// whichever way the list runs: an unknown time is not an old one,
			// and reversing the list must not promote it to the top.
			if ta.IsZero() != tb.IsZero() {
				return tb.IsZero()
			}
			if !ta.IsZero() && !ta.Equal(tb) {
				if desc {
					return ta.After(tb)
				}
				return ta.Before(tb)
			}
			return a.rank < b.rank
		}
		c := a.rank - b.rank
		if key == "source" {
			c = strings.Compare(a.result.Primary.SourceID, b.result.Primary.SourceID)
		}
		if c != 0 {
			if desc {
				return c > 0
			}
			return c < 0
		}
		// Relevance is the tie-break under every key, because it is the one
		// ordering recall actually computed.
		return a.rank < b.rank
	})
}

// sidecarUpdated is the most specific time a candidate carries. The four
// timestamps answer different questions, so they are tried in order of what
// "updated" means to a reader — when the thing happened, then when a source
// boundary confirmed it, then when recall read it — and never averaged.
func sidecarUpdated(c recall.Candidate) time.Time {
	for _, t := range []*time.Time{c.EventTime, c.ConfirmedAt, c.ObservedAt, c.ValidFrom} {
		if t != nil && !t.IsZero() {
			return *t
		}
	}
	return time.Time{}
}

func sidecarResultItem(row sidecarRow) sidecarItem {
	primary := row.result.Primary
	title := primary.Title
	if strings.TrimSpace(title) == "" {
		title = primary.Locator.Local
	}
	item := sidecarItem{
		ID: primary.Locator.String(),
		Cells: map[string]string{
			"rank":   strconv.Itoa(row.rank),
			"title":  sidecarLine(title, 512),
			"source": sidecarLine(primary.SourceID, 512),
			// The excerpt travels as its text alone. Recall marks an excerpt
			// with what it is — the span that matched, or the record's opening
			// shown because nothing matched — and a table cell has nowhere to
			// put that without inventing a glyph the host cannot explain. The
			// distinction is not lost: it is a field on the document `get`
			// returns.
			"excerpt": sidecarLine(primary.Excerpt, 512),
		},
	}
	if row.result.Explanation.ExactPromoted {
		item.Status = &sidecarStatus{Label: "exact", Tone: "success"}
	} else if n := row.result.Explanation.Corroboration.IndependentUnits; n > 1 {
		item.Status = &sidecarStatus{Label: "corroborated " + strconv.Itoa(n), Tone: "info"}
	}
	return item
}

// sidecarOutcomeOf maps recall's two independent facts onto the protocol's one
// enumeration.
//
// Coverage wins over abstention, for the reason recall's own exit codes order
// them that way: an abstention is a claim about the corpus, and an incomplete
// set of sources does not support one.
func sidecarOutcomeOf(resp recall.QueryResponse) string {
	switch {
	case resp.Coverage == recall.CoverageDegraded:
		return "degraded"
	case resp.Outcome == recall.OutcomeAbstained:
		return "abstained"
	default:
		return "answered"
	}
}

func sidecarFailedMessage(resp recall.QueryResponse) string {
	degraded := degradedSources(resp)
	if len(degraded) == 0 {
		return "every source recall asked failed, so an empty list would claim nothing"
	}
	return "every source recall asked failed: " + strings.Join(degraded, ", ")
}

// sidecarNotices carries the coverage facts recall refuses to leave silent:
// which sources did not answer, what was withheld, and what a budget removed.
// Each is one line, and the list is bounded to what the host will draw.
func sidecarNotices(resp recall.QueryResponse) []sidecarNotice {
	var out []sidecarNotice
	add := func(tone, text string) {
		if len(out) >= 4 || text == "" {
			return
		}
		out = append(out, sidecarNotice{Tone: tone, Text: sidecarLine(text, 200)})
	}

	if degraded := degradedSources(resp); len(degraded) > 0 {
		add("warning", fmt.Sprintf("%d of %d sources did not answer (%s)",
			len(degraded), sidecarSourceCount(resp), strings.Join(degraded, ", ")))
	}
	if summary := sidecarSuppressed(resp.Suppressed); summary != "" {
		add("info", summary)
	}
	if resp.DroppedResults > 0 {
		add("info", fmt.Sprintf("%d results dropped by the response budget", resp.DroppedResults))
	}
	for _, omission := range resp.Omitted {
		add("info", "omitted for the response budget: "+string(omission))
	}
	return out
}

func sidecarSourceCount(resp recall.QueryResponse) int {
	if n := len(resp.SourceOutcomes); n > 0 {
		return n
	}
	if resp.SourceSummary != nil {
		return resp.SourceSummary.Sources
	}
	return 0
}

// sidecarSuppressed says how many records were withheld and why. A count with
// no reason would read as a censored answer; a reason with no count would not
// say how much.
func sidecarSuppressed(in []recall.Suppression) string {
	if len(in) == 0 {
		return ""
	}
	counts := map[string]int{}
	var order []string
	total := 0
	for _, s := range in {
		if _, seen := counts[s.Reason]; !seen {
			order = append(order, s.Reason)
		}
		counts[s.Reason] += s.Count
		total += s.Count
	}
	parts := make([]string, 0, len(order))
	for _, reason := range order {
		parts = append(parts, fmt.Sprintf("%s %d", reason, counts[reason]))
	}
	return fmt.Sprintf("%d records suppressed (%s)", total, strings.Join(parts, ", "))
}

// sidecarListSources maps the configured source instances onto rows.
func sidecarListSources(ctx context.Context, env Env, profile string) sidecarResponse {
	listing, resp, ok := sidecarSourceListing(ctx, env, profile)
	if !ok {
		return resp
	}

	items := make([]sidecarItem, 0, len(listing.Sources))
	var unhealthy []string
	for _, s := range listing.Sources {
		items = append(items, sidecarSourceItem(s))
		if s.Probed && (s.Error != "" || s.Health == nil || !s.Health.Usable()) {
			unhealthy = append(unhealthy, s.SourceID)
		}
	}

	page := &sidecarPage{Outcome: "answered", Items: items, Total: len(items)}
	if len(unhealthy) > 0 {
		page.Outcome = "degraded"
		page.Notices = []sidecarNotice{{
			Tone: "warning",
			Text: sidecarLine(fmt.Sprintf("%d of %d sources cannot answer (%s)",
				len(unhealthy), len(items), strings.Join(unhealthy, ", ")), 200),
		}}
	}
	return sidecarResponse{Protocol: sidecarProtocol, Page: page}
}

func sidecarSourceListing(ctx context.Context, env Env, profile string) (SourceListing, sidecarResponse, bool) {
	core, closeCore, err := openCore(env, profile, 0, remoteFlags{})
	if err != nil {
		return SourceListing{}, sidecarUnconfigured(
			"recall could not open its configuration: "+err.Error(), "Run recall doctor"), false
	}
	defer func() { _ = closeCore() }()

	raw, err := core.Sources(ctx)
	if err != nil {
		return SourceListing{}, sidecarFail(sidecarCodeUnavailable,
			"recall could not list its sources: "+err.Error(), true), false
	}
	var listing SourceListing
	if err := listingInto(raw.Payload, &listing); err != nil {
		return SourceListing{}, sidecarFail(sidecarCodeInternal,
			"recall's source listing did not decode: "+err.Error(), false), false
	}
	return listing, sidecarResponse{}, true
}

func sidecarSourceItem(s SourceStatus) sidecarItem {
	return sidecarItem{
		ID: s.SourceID,
		Cells: map[string]string{
			"name":   sidecarLine(s.SourceID, 512),
			"health": sidecarHealthLabel(s),
			"fresh":  sidecarFreshness(s),
		},
		Status: &sidecarStatus{Label: sidecarHealthLabel(s), Tone: sidecarHealthTone(s)},
	}
}

// sidecarHealthLabel says what is known, including that nothing is. A source
// the configuration told recall not to contact reports "not probed" rather than
// an empty cell, which would read as a source that was asked and said nothing.
func sidecarHealthLabel(s SourceStatus) string {
	switch {
	case !s.Enabled:
		return "disabled"
	case !s.Probed:
		return "not probed"
	case s.Error != "":
		return "error"
	case s.Health == nil:
		return "unknown"
	default:
		return string(s.Health.Status)
	}
}

func sidecarHealthTone(s SourceStatus) string {
	switch {
	case !s.Enabled, !s.Probed:
		return "neutral"
	case s.Error != "", s.Health == nil:
		return "danger"
	}
	switch s.Health.Status {
	case recall.HealthHealthy:
		return "success"
	case recall.HealthDegraded:
		return "warning"
	default:
		return "danger"
	}
}

// sidecarFreshness is the most recent instant this source is known to have been
// complete, as RFC 3339. The host owns rendering it as an age.
func sidecarFreshness(s SourceStatus) string {
	if s.Health == nil {
		return ""
	}
	if s.Health.LastSuccess != nil && !s.Health.LastSuccess.IsZero() {
		return s.Health.LastSuccess.UTC().Format(time.RFC3339)
	}
	if !s.Health.CheckedAt.IsZero() {
		return s.Health.CheckedAt.UTC().Format(time.RFC3339)
	}
	return ""
}

func sidecarGet(ctx context.Context, env Env, profile string, req sidecarRequest) sidecarResponse {
	var params sidecarGetParams
	if resp, ok := sidecarParams(req.Params, &params); !ok {
		return resp
	}
	switch params.Collection {
	case sidecarResults:
		return sidecarGetResult(ctx, env, profile, params.ID, req.Context)
	case sidecarSources:
		return sidecarGetSource(ctx, env, profile, params.ID)
	default:
		return sidecarUnknownCollection(params.Collection)
	}
}

// sidecarGetResult expands one locator into its evidence.
//
// What the document can say is bounded by what expansion knows. Expansion is
// stateless with respect to the query that produced the locator — deliberately,
// because it re-checks permissions and must not trust a caller's memory of a
// result — and `get` carries only the collection and the row id. So the title,
// the record's date, the corroboration count, and the exact/corroborated pill
// the row carried are not reachable here, and this document does not invent
// them.
func sidecarGetResult(ctx context.Context, env Env, profile, id string, reqCtx *sidecarContext) sidecarResponse {
	if _, refusal := sidecarProjectScope(reqCtx); refusal != nil {
		return *refusal
	}
	locator, err := recall.ParseLocator(strings.TrimSpace(id))
	if err != nil {
		return sidecarFail(sidecarCodeInvalidRequest, err.Error(), false)
	}

	core, closeCore, err := openCore(env, profile, 0, remoteFlags{})
	if err != nil {
		return sidecarUnconfigured("recall could not open its configuration: "+err.Error(),
			"Run recall doctor")
	}
	defer func() { _ = closeCore() }()

	resp, err := core.Expand(ctx, recall.ExpandRequest{
		Locator: locator,
		Detail:  recall.DetailFull,
		// The host cuts a body at 64 KiB, so asking for more would be asking
		// a source to read bytes nobody will see.
		Budget: 64 * 1024,
	})
	if err != nil {
		// An unconfigured source, a denied one, an expired locator, or an
		// unreachable one is a source failure and never an empty document.
		return sidecarFail(sidecarCodeUnavailable, "recall could not expand "+
			locator.String()+": "+err.Error(), true)
	}

	doc := &sidecarDocument{
		Identity: locator.String(),
		Title:    sidecarLine(locator.Local, 300),
		Subtitle: sidecarLine(locator.SourceID, 120),
		Fields: []sidecarField{
			{Label: "Source", Value: sidecarLine(locator.SourceID, 512)},
			{Label: "Locator", Value: sidecarLine(locator.String(), 512)},
		},
		Sections: []sidecarSection{{
			Title: "Evidence",
			Body:  &sidecarBody{Format: "markdown", Text: resp.Content},
		}},
	}
	if provenance := sidecarProvenanceFields(resp); len(provenance) > 0 {
		doc.Sections = append(doc.Sections, sidecarSection{Title: "Provenance", Fields: provenance})
	}
	return sidecarResponse{Protocol: sidecarProtocol, Resource: doc}
}

// sidecarProvenanceFields is where the content came from and what was cut. A
// reader has to be able to see the path and the revision before trusting the
// text above them.
func sidecarProvenanceFields(resp recall.ExpandResponse) []sidecarField {
	var out []sidecarField
	add := func(label, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		out = append(out, sidecarField{Label: label, Value: sidecarLine(value, 512)})
	}
	add("Provenance", resp.Provenance)
	add("Revision", resp.SourceRevision)
	if resp.Truncated {
		add("Truncated", "yes")
		add("Boundary", resp.TruncationBoundary)
	}
	return out
}

// sidecarGetSource is one source instance as an operator sees it.
func sidecarGetSource(ctx context.Context, env Env, profile, id string) sidecarResponse {
	listing, resp, ok := sidecarSourceListing(ctx, env, profile)
	if !ok {
		return resp
	}
	for _, s := range listing.Sources {
		if s.SourceID != id {
			continue
		}
		return sidecarResponse{Protocol: sidecarProtocol, Resource: sidecarSourceDocument(listing, s)}
	}
	return sidecarFail(sidecarCodeNotFound, fmt.Sprintf(
		"no source named %q in profile %q", id, listing.Profile), false)
}

func sidecarSourceDocument(listing SourceListing, s SourceStatus) *sidecarDocument {
	doc := &sidecarDocument{
		Identity: s.SourceID,
		Title:    sidecarLine(s.SourceID, 300),
		Subtitle: sidecarLine(strings.TrimSpace(s.Adapter+" · "+string(s.FreshnessMode)), 120),
		Status:   &sidecarStatus{Label: sidecarHealthLabel(s), Tone: sidecarHealthTone(s)},
		Fields: []sidecarField{
			{Label: "Adapter", Value: sidecarLine(s.Adapter, 512)},
			{Label: "Profile", Value: sidecarLine(listing.Profile, 512)},
			{Label: "Enabled", Value: sidecarYesNo(s.Enabled)},
			{Label: "In profile", Value: sidecarYesNo(s.InProfile)},
			{Label: "Sensitivity", Value: sidecarLine(s.Sensitivity, 512)},
		},
	}
	if s.Location != "" {
		doc.Fields = append(doc.Fields, sidecarField{Label: "Location", Value: sidecarLine(s.Location, 512)})
	}

	if health := sidecarHealthFields(s); len(health) > 0 {
		doc.Sections = append(doc.Sections, sidecarSection{Title: "Health", Fields: health})
	}
	if caps := sidecarCapabilityFields(s); len(caps) > 0 {
		doc.Sections = append(doc.Sections, sidecarSection{Title: "Capabilities", Fields: caps})
	}
	return doc
}

func sidecarHealthFields(s SourceStatus) []sidecarField {
	var out []sidecarField
	add := func(label, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		out = append(out, sidecarField{Label: label, Value: sidecarLine(value, 512)})
	}
	add("Status", sidecarHealthLabel(s))
	if s.Error != "" {
		add("Error", s.Error)
	}
	if !s.Probed {
		// Saying so is the point: an unprobed source has no health, and a
		// blank block would read as one that reported nothing.
		add("Probed", "no; the configuration says not to contact it")
		return out
	}
	if s.Health == nil {
		return out
	}
	add("Coverage", string(s.Health.Coverage))
	if !s.Health.CheckedAt.IsZero() {
		add("Checked", s.Health.CheckedAt.UTC().Format(time.RFC3339))
	}
	if s.Health.LastSuccess != nil && !s.Health.LastSuccess.IsZero() {
		add("Last success", s.Health.LastSuccess.UTC().Format(time.RFC3339))
	}
	add("Source watermark", s.Health.SourceWatermark)
	add("Index watermark", s.Health.IndexWatermark)
	add("Generation", s.Health.IndexGeneration)
	if s.Health.RecordCount > 0 {
		add("Records", strconv.FormatInt(s.Health.RecordCount, 10))
	}
	if s.Health.IndexedCount > 0 {
		add("Indexed", strconv.FormatInt(s.Health.IndexedCount, 10))
	}
	if s.Health.FailedCount > 0 {
		add("Failed", strconv.FormatInt(s.Health.FailedCount, 10))
	}
	return out
}

func sidecarCapabilityFields(s SourceStatus) []sidecarField {
	var out []sidecarField
	add := func(label, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		out = append(out, sidecarField{Label: label, Value: sidecarLine(value, 512)})
	}
	add("Adapter id", s.AdapterID)
	add("Name", s.DisplayName)
	add("Capabilities", join(s.Capabilities))
	add("Query modes", join(s.QueryModes))
	add("as_of", string(s.AsOfSupport))
	add("Record types", join(s.RecordTypes))
	add("Freshness policy", s.FreshnessPolicy)
	return out
}

func sidecarYesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// sidecarAct performs one typed operation. It is the only method that mutates,
// and the only one the host confirms with the user first.
func sidecarAct(ctx context.Context, env Env, profile string, req sidecarRequest) sidecarResponse {
	var params sidecarActParams
	if resp, ok := sidecarParams(req.Params, &params); !ok {
		return resp
	}
	if params.Action != "refresh-source" {
		return sidecarFail(sidecarCodeInvalidRequest, fmt.Sprintf(
			"no action named %q; recall declares refresh-source", params.Action), false)
	}
	if params.Collection != sidecarSources {
		return sidecarFail(sidecarCodeInvalidRequest, fmt.Sprintf(
			"refresh-source applies to the %s collection, not %q", sidecarSources, params.Collection), false)
	}
	id := strings.TrimSpace(params.ID)
	if id == "" {
		return sidecarFail(sidecarCodeInvalidRequest, "refresh-source needs a source id", false)
	}

	core, closeCore, err := openCore(env, profile, 0, remoteFlags{})
	if err != nil {
		return sidecarUnconfigured("recall could not open its configuration: "+err.Error(),
			"Run recall doctor")
	}
	defer func() { _ = closeCore() }()

	resp, err := core.Refresh(ctx, recall.RefreshRequest{Profile: profile, SourceID: id})
	if err != nil {
		// A failed action is a typed failure with a message, not a transport
		// failure: the user asked for something that did not happen, and they
		// need to be told which.
		return sidecarResponse{Protocol: sidecarProtocol, Outcome: &sidecarOutcome{
			Status:  "failed",
			Message: sidecarLine("recall could not refresh "+id+": "+err.Error(), 200),
		}}
	}

	outcome := &sidecarOutcome{
		Status:  "done",
		Message: sidecarLine(sidecarRefreshMessage(id, resp), 200),
		Refresh: []string{sidecarSources},
		Open:    &sidecarOpen{Collection: sidecarSources, ID: id},
	}
	if resp.Outcome == recall.RefreshFailed {
		outcome.Status = "failed"
		outcome.Open = nil
	}
	return sidecarResponse{Protocol: sidecarProtocol, Outcome: outcome}
}

func sidecarRefreshMessage(id string, resp recall.RefreshResponse) string {
	for _, s := range resp.Sources {
		if s.SourceID != id {
			continue
		}
		msg := fmt.Sprintf("%s: %s", id, s.Status)
		if s.Reason != "" {
			msg += " (" + string(s.Reason) + ")"
		}
		if s.Health != nil {
			msg += ", health " + string(s.Health.Status)
		}
		return msg
	}
	return fmt.Sprintf("%s: %s", id, resp.Outcome)
}

// sidecarLine bounds one string to a single line of at most max runes. Every
// string this plugin sends goes through it: the host has its own limits and
// truncates rather than refusing, but a plugin that sent a newline inside a
// table cell would be asking the host to decide what it meant.
func sidecarLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max-1])) + "…"
}

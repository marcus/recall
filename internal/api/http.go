package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/marcus/recall/internal/buildinfo"
	"github.com/marcus/recall/internal/pointer"
	"github.com/marcus/recall/internal/recall"
)

// Version is the API version in every path. It changes when a response shape
// changes incompatibly, so a client pinned to /v1 keeps working or fails
// loudly, and never silently reads a field that came to mean something else.
const Version = "v1"

// MaxRequestBytes bounds one request body.
//
// Requests here are small by construction: a query is text plus a scope, and an
// expansion is a locator plus a budget. Anything approaching this limit is a
// client fault or an attempt to make the server allocate, and reading it to
// find out would be doing the allocating.
const MaxRequestBytes = 1 << 20

// DefaultRequestTimeout bounds one request end to end.
//
// The plan already gives every source its own deadline, so this is a backstop
// rather than the mechanism: it bounds the whole request including fusion and
// serialization, so a caller that set no budget still cannot pin a connection
// open indefinitely.
const DefaultRequestTimeout = 2 * time.Minute

// ServerOptions configure the HTTP handler.
type ServerOptions struct {
	Core Core

	// BearerToken, when non-empty, authenticates every request. A non-loopback
	// listener is forbidden unless this is configured.
	BearerToken string

	// RequestTimeout bounds one request. Zero means [DefaultRequestTimeout].
	RequestTimeout time.Duration

	// Log receives one line per request. It is optional, and what it may
	// contain is constrained: see [Handler.log].
	Log func(string)
}

// Handler is the local HTTP API.
type Handler struct {
	core    Core
	token   string
	timeout time.Duration
	log     func(string)
	mux     *http.ServeMux
}

// NewHandler builds the routed, guarded HTTP surface.
//
// Routing is explicit per method so an unmatched method is a 405 naming what is
// allowed, rather than a 404 that reads as "this API does not have that
// endpoint" when it does.
func NewHandler(opt ServerOptions) *Handler {
	h := &Handler{
		core:    opt.Core,
		token:   opt.BearerToken,
		timeout: opt.RequestTimeout,
		log:     opt.Log,
		mux:     http.NewServeMux(),
	}
	if h.timeout <= 0 {
		h.timeout = DefaultRequestTimeout
	}

	h.mux.HandleFunc("POST /"+Version+"/query", h.handleQuery)
	h.mux.HandleFunc("POST /"+Version+"/expand", h.handleExpand)
	h.mux.HandleFunc("POST /"+Version+"/refresh", h.handleRefresh)
	h.mux.HandleFunc("GET /"+Version+"/sources", h.handleSources)
	h.mux.HandleFunc("GET /"+Version+"/doctor", h.handleDoctor)
	h.mux.HandleFunc("GET /"+Version+"/version", h.handleVersion)
	h.mux.HandleFunc("/", h.handleUnknown)

	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}

	if problem, status := guard(r, h.token); problem != nil {
		if status == http.StatusUnauthorized {
			rec.Header().Set("WWW-Authenticate", `Bearer realm="recall"`)
		}
		writeProblem(rec, status, *problem)
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
		defer cancel()
		r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
		rec.Header().Set(HeaderProfile, h.core.Profile())
		h.mux.ServeHTTP(rec, r.WithContext(ctx))
	}

	// One line per request, carrying method, path, status, and duration — and
	// nothing else. Query text and excerpts are not logged by default anywhere
	// in Recall, and a request log is the easiest place to forget that: the
	// text is right there on the request. A log that recorded it would turn
	// every search into a durable record of what somebody was looking for.
	if h.log != nil {
		h.log(fmt.Sprintf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(started).Round(time.Millisecond)))
	}
}

func (h *Handler) handleQuery(w http.ResponseWriter, r *http.Request) {
	// The wire shape is the domain request plus the tier the caller wants back,
	// which is a property of this transport rather than of a query: nothing
	// about the search changes, only how much of the answer is serialized.
	var body struct {
		recall.QueryRequest
		Tier string `json:"tier,omitempty"`
	}
	if problem := decode(r, &body); problem != nil {
		writeProblem(w, http.StatusBadRequest, *problem)
		return
	}
	req := body.QueryRequest
	tier, problem := queryTier(body.Tier)
	if problem != nil {
		writeProblem(w, http.StatusBadRequest, *problem)
		return
	}
	// What this handler writes is what an undeclared budget is charged against:
	// the whole serialized response, or the projection when one was asked for.
	// A client that renders a further projection of either says so and is
	// priced for that instead.
	if problem := normalizeQuery(&req, h.core.Profile(), tier); problem != nil {
		writeProblem(w, requestStatus(*problem), *problem)
		return
	}

	resp, err := h.core.Query(r.Context(), req)
	if err != nil {
		if errors.Is(err, recall.ErrUnsatisfiableScope) {
			// The caller's to fix, not this installation's: a scope naming
			// sources this profile does not contain. 400 rather than 500, and
			// the message carries the profile that would serve it.
			writeProblem(w, http.StatusBadRequest, Problem{CodeBadRequest, err.Error()})
			return
		}
		// A request that could not be planned or fused made no claim about the
		// corpus at all. It is reported as a Recall failure rather than as an
		// empty answer, because an empty answer would be a statement about what
		// is in the sources and nothing here looked. The message is safe to
		// carry: planning and fusion fail on configuration Recall resolved, not
		// on anything a source said.
		writeProblem(w, http.StatusInternalServerError,
			Problem{CodeInternal, "the request could not be planned or fused: " + err.Error()})
		return
	}

	severity := Severity(resp)
	w.Header().Set(HeaderOutcome, string(resp.Outcome))
	w.Header().Set(HeaderCoverage, string(resp.Coverage))
	if tier == recall.SurfaceStructuredPointer {
		writeJSON(w, HTTPStatus(severity), pointer.Project(resp))
		return
	}
	writeJSON(w, HTTPStatus(severity), resp)
}

// queryTier reads the requested body tier.
//
// The default is the complete serialization, and stays that way where the MCP
// tool's default became the projection. The asymmetry is deliberate and is
// about who is holding the contract. A tool description is read fresh by a
// model on every session, so changing what it returns is a change to a prompt;
// /v1 is a pinned API whose whole promise is that a client reading it keeps
// working, and quietly halving the response shape is exactly the incompatible
// change the version number exists to make loud. `recall query --server` is one
// such client: it decodes the domain type and projects locally, so it must keep
// receiving the whole of it.
//
// tier is separate from budget.surface because the two answer different
// questions. surface is what the CALLER will consume — which may be a
// projection this server never made, as it is for the CLI over a socket — and
// tier is what this server SENDS. Reading a pricing declaration as an
// instruction to project would have made the CLI's own remote path
// undecodable.
func queryTier(tier string) (recall.ResponseSurface, *Problem) {
	switch tier {
	case "", "complete":
		return recall.SurfaceStructured, nil
	case "pointer":
		return recall.SurfaceStructuredPointer, nil
	default:
		return "", &Problem{CodeBadRequest, fmt.Sprintf(
			"tier %q: want %q for the whole response or %q for one pointer per result", tier, "complete", "pointer")}
	}
}

func (h *Handler) handleExpand(w http.ResponseWriter, r *http.Request) {
	// The wire shape is the domain type plus the profile the caller believes it
	// is talking to, so a client holding a locator from another profile is told
	// so instead of being served from this one.
	var body struct {
		recall.ExpandRequest
		Profile string `json:"profile,omitempty"`
	}
	if problem := decode(r, &body); problem != nil {
		writeProblem(w, http.StatusBadRequest, *problem)
		return
	}
	if problem := checkProfile(body.Profile, h.core.Profile()); problem != nil {
		writeProblem(w, requestStatus(*problem), *problem)
		return
	}
	req := body.ExpandRequest
	if problem := normalizeExpand(&req); problem != nil {
		writeProblem(w, requestStatus(*problem), *problem)
		return
	}

	resp, err := h.core.Expand(r.Context(), req)
	if err != nil {
		problem := classify(err)
		writeProblem(w, expandStatus(problem.Code), problem)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req recall.RefreshRequest
	if problem := decode(r, &req); problem != nil {
		writeProblem(w, http.StatusBadRequest, *problem)
		return
	}
	if problem := normalizeRefresh(&req, h.core.Profile()); problem != nil {
		writeProblem(w, requestStatus(*problem), *problem)
		return
	}
	resp, err := h.core.Refresh(r.Context(), req)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError,
			Problem{CodeInternal, "the refresh could not be planned: " + err.Error()})
		return
	}
	status := refreshStatus(resp.Outcome)
	w.Header().Set(HeaderOutcome, string(resp.Outcome))
	writeJSON(w, HTTPStatus(status), resp)
}

func refreshStatus(outcome recall.RefreshOutcome) Status {
	switch outcome {
	case recall.RefreshSucceeded:
		return StatusOK
	case recall.RefreshDegraded:
		return StatusDegraded
	default:
		return StatusFailed
	}
}

func (h *Handler) handleSources(w http.ResponseWriter, r *http.Request) {
	listing, err := h.core.Sources(r.Context())
	writeListing(w, listing, err)
}

func (h *Handler) handleDoctor(w http.ResponseWriter, r *http.Request) {
	listing, err := h.core.Doctor(r.Context())
	writeListing(w, listing, err)
}

// writeListing emits a listing under the same status mapping a query gets, so
// "this installation is degraded" reads identically whichever endpoint said it.
func writeListing(w http.ResponseWriter, listing Listing, err error) {
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, Problem{CodeInternal, "the listing could not be produced"})
		return
	}
	w.Header().Set(HeaderCoverage, string(listing.Status))
	writeJSON(w, HTTPStatus(listing.Status), listing.Payload)
}

// Identity is what /v1/version answers: enough for a client to confirm it is
// talking to the build and profile it thinks it is before trusting an answer.
type Identity struct {
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	APIVersion string `json:"api_version"`
	Profile    string `json:"profile"`
}

func (h *Handler) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Identity{
		Version:    buildinfo.Version,
		Commit:     buildinfo.Commit,
		APIVersion: Version,
		Profile:    h.core.Profile(),
	})
}

// handleUnknown answers everything the routes did not.
//
// A path that exists under a different method gets 405 with an Allow header;
// anything else gets 404. Both carry the same problem body as every other
// failure, so a client has exactly one error shape to parse.
func (h *Handler) handleUnknown(w http.ResponseWriter, r *http.Request) {
	if allowed, ok := methodsFor(r.URL.Path); ok {
		w.Header().Set("Allow", allowed)
		writeProblem(w, http.StatusMethodNotAllowed, Problem{CodeNotFound,
			fmt.Sprintf("%s is not allowed on %s; use %s", r.Method, r.URL.Path, allowed)})
		return
	}
	writeProblem(w, http.StatusNotFound, Problem{CodeNotFound,
		fmt.Sprintf("no such endpoint: %s. This API serves %s", r.URL.Path, strings.Join(Endpoints(), ", "))})
}

// Endpoints lists every path this API serves, so a 404 can tell a caller what
// it could have asked for instead of only what it could not.
func Endpoints() []string {
	names := []string{"query", "expand", "refresh", "sources", "doctor", "version"}
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, "/"+Version+"/"+name)
	}
	return out
}

func methodsFor(path string) (string, bool) {
	switch path {
	case "/" + Version + "/query", "/" + Version + "/expand", "/" + Version + "/refresh":
		return http.MethodPost, true
	case "/" + Version + "/sources", "/" + Version + "/doctor", "/" + Version + "/version":
		return http.MethodGet, true
	default:
		return "", false
	}
}

// requestStatus maps a fault in the request itself onto a status code.
func requestStatus(p Problem) int {
	if p.Code == CodeProfileMismatch {
		// The request is well formed and this server is the wrong one to send
		// it to. 409 says that; 400 would suggest the body needs fixing.
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// expandStatus maps an expansion failure onto a status code.
//
// The distinctions matter to a caller deciding what to do next. A denial is
// permanent and about policy; an expired locator is permanent and about the
// record; an unreachable source is transient and about the machine. Collapsing
// them into one code would make "try again" and "stop asking" look the same.
func expandStatus(code string) int {
	switch code {
	case CodeMalformedLocator:
		return http.StatusBadRequest
	case CodeDenied:
		return http.StatusForbidden
	case CodeNotConfigured, CodeUnknownLocator:
		return http.StatusNotFound
	case CodeExpiredLocator:
		return http.StatusGone
	case CodeTimeout:
		return http.StatusGatewayTimeout
	default:
		// Unreachable, budget_exceeded, failed: the source, not Recall, could
		// not serve the request. 502 says the failure is upstream of here.
		return http.StatusBadGateway
	}
}

// decode reads a JSON body, refusing anything it does not fully understand.
//
// Unknown fields are an error rather than something to ignore. A caller that
// misspells "scope" believes it narrowed the request; ignoring the field would
// search sources it thought it had excluded and return a confident answer to a
// question nobody asked. The same reasoning is why the CLI fails on a mistyped
// --scope instead of dropping it.
func decode(r *http.Request, v any) *Problem {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &Problem{CodeBadRequest, fmt.Sprintf("request body exceeds %d bytes", MaxRequestBytes)}
		}
		return &Problem{CodeBadRequest, "request body is not the JSON this endpoint accepts: " + err.Error()}
	}
	// A second value in the stream means the caller sent something other than
	// one request, and guessing which one it meant is not this layer's job.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return &Problem{CodeBadRequest, "request body must contain exactly one JSON object"}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		// Marshalling a response Recall built is a bug in Recall, not a source
		// failure, so it is never reported as one.
		writeProblem(w, http.StatusInternalServerError, Problem{CodeInternal, "the response could not be serialized"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeProblem(w http.ResponseWriter, status int, p Problem) {
	body, _ := json.MarshalIndent(struct {
		Error Problem `json:"error"`
	}{p}, "", "  ")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// recorder remembers the status a handler wrote, so the request log can report
// it without the handlers having to hand it back.
type recorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *recorder) WriteHeader(status int) {
	if !r.written {
		r.status, r.written = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

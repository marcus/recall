package ongoing_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/marcus/recall/cmd/recall-ongoing/ongoing"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// The tests here exercise the live transport, which the `replay` setting
// deliberately bypasses: authentication, the read-only boundary, and what a
// dead host looks like are properties of talking to an instance, and a
// recording cannot have them.

const testSecret = "0123456789abcdef0123456789abcdef"

// instance is a stand-in ongoing server that records what was asked of it.
type instance struct {
	*httptest.Server

	mu       sync.Mutex
	calls    []string // "METHOD path", in order
	requires bool     // demand a session cookie for /api/projects
	secret   string
	issued   string
}

func newInstance(t *testing.T, requiresAuth bool) *instance {
	t.Helper()
	inst := &instance{requires: requiresAuth, secret: testSecret, issued: "sess-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		inst.record(r)
		writeJSON(w, http.StatusOK, `{"ok":true}`)
	})
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		inst.record(r)
		if inst.requires && !inst.hasSession(r) {
			writeJSON(w, http.StatusUnauthorized, `{"error":"Authentication required"}`)
			return
		}
		writeJSON(w, http.StatusOK, catalogFixture)
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		inst.record(r)
		if err := r.ParseForm(); err != nil || r.PostFormValue("secret") != inst.secret {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if origin := r.Header.Get("Origin"); origin != inst.URL {
			// ongoing checks the origin of every mutation whenever
			// authentication is required, so an adapter that did not send one
			// would be refused by the real instance.
			w.WriteHeader(http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "ongoing_session", Value: inst.issued, Path: "/"})
		w.Header().Set("Location", "/")
		w.WriteHeader(http.StatusSeeOther)
	})
	inst.Server = httptest.NewServer(mux)
	t.Cleanup(inst.Close)
	return inst
}

func (i *instance) record(r *http.Request) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls = append(i.calls, r.Method+" "+r.URL.Path)
}

func (i *instance) hasSession(r *http.Request) bool {
	c, err := r.Cookie("ongoing_session")
	return err == nil && c.Value == i.issued
}

func (i *instance) seen() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.calls...)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// live hands the adapter a handshake pointed at a real HTTP endpoint.
func live(t *testing.T, url string, settings map[string]any) *ongoing.Adapter {
	t.Helper()
	a := ongoing.New(ongoing.Options{Clock: fixedClock})
	t.Cleanup(func() { _ = a.Close() })
	if _, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            t.TempDir(),
		SourceID:           "ongoing",
		Location:           url,
		Settings:           settings,
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return a
}

func TestTheAccessSecretIsReadFromTheEnvironmentAndUsedToLogIn(t *testing.T) {
	// ongoing takes its shared secret from the environment and so does this
	// adapter. There is no settings key for it: a settings block travels inside
	// a committed configuration file, and the schema refuses unknown keys.
	inst := newInstance(t, true)
	t.Setenv(ongoing.SecretEnvVar, testSecret)

	a := live(t, inst.URL, nil)
	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "hnbooks", Limit: 5, Deadline: soon(),
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("search returned %d candidates after logging in", len(resp.Candidates))
	}

	// One refusal, one login, one retry — and then the session is reused
	// rather than re-established per request.
	if _, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "recall", Limit: 5, Deadline: soon(),
	}); err != nil {
		t.Fatalf("second search: %v", err)
	}
	logins := 0
	for _, call := range inst.seen() {
		if call == "POST /login" {
			logins++
		}
	}
	if logins != 1 {
		t.Errorf("logged in %d times across two searches, want once", logins)
	}
}

func TestWithoutTheEnvironmentSecretARequiringInstanceIsDenied(t *testing.T) {
	// No secret to offer, and the instance wants one. That is a denial, never
	// an empty result, and health says which side of the boundary to look at.
	inst := newInstance(t, true)
	t.Setenv(ongoing.SecretEnvVar, "")

	a := live(t, inst.URL, nil)
	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "hnbooks", Limit: 5, Deadline: soon(),
	})
	if !errors.Is(err, protocol.ErrSourceDenied) {
		t.Fatalf("search error = %v, want source_denied", err)
	}
	if resp.Outcome != recall.SearchDenied || len(resp.Candidates) != 0 {
		t.Errorf("outcome = %q with %d candidates", resp.Outcome, len(resp.Candidates))
	}
	for _, call := range inst.seen() {
		if call == "POST /login" {
			t.Error("the adapter attempted a login with no secret to send")
		}
	}

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthDenied {
		t.Fatalf("health = %q, want denied", health.Status)
	}
	if got := health.Diagnostics["access_secret_configured"]; got != false {
		t.Errorf("access_secret_configured = %v, want false", got)
	}
}

func TestARefusedSecretIsDeniedAndNotRetriedForever(t *testing.T) {
	inst := newInstance(t, true)
	t.Setenv(ongoing.SecretEnvVar, "wrong-secret-wrong-secret")

	a := live(t, inst.URL, nil)
	_, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "hnbooks", Limit: 5, Deadline: soon(),
	})
	if !errors.Is(err, protocol.ErrSourceDenied) {
		t.Fatalf("search error = %v, want source_denied", err)
	}
	logins := 0
	for _, call := range inst.seen() {
		if call == "POST /login" {
			logins++
		}
	}
	if logins != 1 {
		t.Errorf("attempted %d logins for one refused secret, want one", logins)
	}
}

func TestOnlyReadsAreEverIssuedAgainstTheCatalog(t *testing.T) {
	// ongoing's PATCH and POST routes edit the owner's catalog. A retrieval
	// source that could write is one that could be talked into writing, so the
	// only requests this adapter makes are GETs of two endpoints — plus the
	// login POST, which carries no query text and no catalog data, and is only
	// issued when the instance demands it.
	inst := newInstance(t, false)
	a := live(t, inst.URL, nil)

	if _, err := a.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	if _, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "recall", Limit: 5, Deadline: soon(),
	}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if _, err := a.Expand(context.Background(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "ongoing", Local: "project_recall"},
		Detail:  recall.DetailFull, Budget: 4096, Deadline: soon(),
	}); err != nil {
		t.Fatalf("expand: %v", err)
	}

	allowed := map[string]bool{"GET /api/health": true, "GET /api/projects": true}
	for _, call := range inst.seen() {
		if !allowed[call] {
			t.Errorf("the adapter issued %q; only reads of the two catalog endpoints are allowed", call)
		}
	}
}

func TestADeadHostIsUnavailableAndSaysNothingAboutTheAddress(t *testing.T) {
	// The address is already in the source's configured location; what a reader
	// needs from a failure is that nothing answered.
	inst := newInstance(t, false)
	url := inst.URL
	inst.Close()

	a := live(t, url, nil)
	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "recall", Limit: 5, Deadline: soon(),
	})
	if !errors.Is(err, protocol.ErrSourceUnavailable) {
		t.Fatalf("search error = %v, want source_unavailable", err)
	}
	if resp.Outcome != recall.SearchUnavailable || len(resp.Candidates) != 0 {
		t.Errorf("outcome = %q with %d candidates", resp.Outcome, len(resp.Candidates))
	}

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthUnavailable {
		t.Errorf("health = %q, want unavailable", health.Status)
	}
}

func TestAnInstanceThatCannotOpenItsCatalogIsUnavailable(t *testing.T) {
	// ongoing answers 503 when the SQLite catalog cannot be opened. The process
	// is up — its public health endpoint says so — and the source still cannot
	// be read, which is exactly the case a bare process probe would miss.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"ok":true}`)
	})
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, `{"error":"The cached catalog could not be opened"}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	a := live(t, server.URL, nil)
	_, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "recall", Limit: 5, Deadline: soon(),
	})
	if !errors.Is(err, protocol.ErrSourceUnavailable) {
		t.Fatalf("search error = %v, want source_unavailable", err)
	}
	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthUnavailable {
		t.Errorf("health = %q, want unavailable", health.Status)
	}
}

func TestAPageModelThatNamesItsOwnLoadErrorIsNotAnEmptyCatalog(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"ok":true}`)
	})
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK,
			`{"generatedAt":"2026-07-24T12:00:00.000Z","loadError":"catalog is locked","scan":null,"projects":[]}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	a := live(t, server.URL, nil)
	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "recall", Limit: 5, Deadline: soon(),
	})
	if !errors.Is(err, protocol.ErrSourceUnavailable) {
		t.Fatalf("search error = %v, want source_unavailable", err)
	}
	if resp.Outcome == recall.SearchSuccess {
		t.Error("a stated load error came back as a successful empty search")
	}
}

func TestACatalogThatHasNeverBeenScannedIsDegradedWithUnknownCoverage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"ok":true}`)
	})
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK,
			`{"generatedAt":"2026-07-24T12:00:00.000Z","loadError":null,"scan":null,"projects":[]}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	a := live(t, server.URL, nil)
	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthDegraded || health.Coverage != recall.IndexUnknown {
		t.Errorf("health = %s/%s, want degraded with unknown coverage",
			health.Status, health.Coverage)
	}
}

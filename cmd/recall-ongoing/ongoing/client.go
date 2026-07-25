package ongoing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The two endpoints this adapter reads, and nothing else.
//
// GET only. ongoing's PATCH and POST routes edit the owner's catalog; a
// retrieval source that could write is a retrieval source that could be talked
// into writing, and invariant 3 says retrieved content never steers what
// Recall does.
const (
	projectsPath = "/api/projects"
	healthPath   = "/api/health"

	// loginPath exchanges the access secret for a session cookie. It is not a
	// read of the source; it is the only request here that is not a GET, and
	// it carries no query text and no catalog data.
	loginPath = "/login"
)

// SecretEnvVar names the environment variable holding ongoing's access secret.
//
// The environment is the only place it is read from. ongoing itself takes it
// this way, a settings block travels in a committed configuration file, and
// the settings schema has additionalProperties:false — so a configuration that
// tried to carry the secret fails the handshake instead of leaking it into a
// repository.
const SecretEnvVar = "ONGOING_ACCESS_SECRET"

// sessionCookie is the cookie ongoing sets after a successful login.
const sessionCookie = "ongoing_session"

// maxBodyBytes bounds one response. The live catalog is a little over a
// megabyte for a hundred repositories; this is generous enough that a real
// answer never hits it and small enough that a runaway server costs a failed
// request rather than this process's memory.
const maxBodyBytes = 64 << 20

// transport fetches one endpoint's body. It exists so the adapter under test
// is the parsing, the ranking, and the freshness arithmetic, over a source
// that a recorded conformance case can hold still.
type transport interface {
	get(ctx context.Context, path string) ([]byte, error)

	// kind names how the answer was obtained, for search and health
	// diagnostics. An answer replayed from a recording is not an answer from
	// the live instance, and a reader has to be able to tell the two apart.
	kind() string
}

// Transport kinds, as they appear in diagnostics.
const (
	transportLive   = "live"
	transportReplay = "replay"
)

// httpTransport talks to a live ongoing instance.
type httpTransport struct {
	base   string
	client *http.Client

	// secret is the access secret from the environment, empty when none was
	// set. A source on loopback needs none; ongoing itself only requires one
	// for a non-loopback listener.
	secret string

	mu      sync.Mutex
	session string
}

func newHTTPTransport(base string, timeout time.Duration, secret string) *httpTransport {
	return &httpTransport{
		base:   strings.TrimSuffix(base, "/"),
		secret: secret,
		client: &http.Client{
			Timeout: timeout,
			// The login response is the thing being read: a 303 carrying
			// Set-Cookie. Following it would discard the cookie's arrival and
			// leave the session unauthenticated for reasons nothing could see.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (t *httpTransport) kind() string { return transportLive }

// get reads one endpoint, authenticating once if the instance asks it to.
//
// The retry is bounded to a single attempt. A session cookie expires, and
// re-logging in once is the difference between a working long-lived adapter
// process and one that has to be restarted daily; retrying further would turn
// a refused secret into a login loop against a rate limiter.
func (t *httpTransport) get(ctx context.Context, path string) ([]byte, error) {
	body, status, err := t.do(ctx, path)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized && t.secret != "" {
		if err := t.login(ctx); err != nil {
			return nil, err
		}
		body, status, err = t.do(ctx, path)
		if err != nil {
			return nil, err
		}
	}
	if status == http.StatusOK {
		return body, nil
	}
	return nil, statusError(status)
}

func (t *httpTransport) do(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+path, nil)
	if err != nil {
		return nil, 0, unreachable(err)
	}
	req.Header.Set("Accept", "application/json")
	t.mu.Lock()
	cookie := t.session
	t.mu.Unlock()
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, 0, unreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, 0, unreachable(err)
	}
	return body, resp.StatusCode, nil
}

// login exchanges the access secret for a session cookie.
//
// ongoing's login is a SvelteKit form action: a form-encoded POST answered
// with 303 and a Set-Cookie on success, and with 400 when the secret is
// refused. The Origin header matches the configured base URL because ongoing
// checks it on every mutation whenever authentication is required.
func (t *httpTransport) login(ctx context.Context) error {
	form := url.Values{"secret": {t.secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+loginPath,
		strings.NewReader(form.Encode()))
	if err != nil {
		return unreachable(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", t.base)

	resp, err := t.client.Do(req)
	if err != nil {
		return unreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck // the body is a rendered page, never read
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))

	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		// 400 is a refused secret and 429 is the login rate limiter. Both are a
		// denial from this adapter's side, and neither message is repeated:
		// anything specific enough to be useful about a refused credential is
		// something the caller did not prove it may know.
		return errDenied
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.mu.Lock()
			t.session = c.Name + "=" + c.Value
			t.mu.Unlock()
			return nil
		}
	}
	return errDenied
}

// replayTransport answers from recorded responses instead of a network.
//
// A conformance transcript has to be replayable on a machine with no ongoing
// instance, and an evaluation pack has to be reproducible against a source that
// cannot change underneath a benchmark. Recording the status alongside the body
// is what lets a case cover denial and unavailability as well as a good answer,
// rather than only the happy path a live recording happens to catch.
//
// It never reads the environment and never logs in: authentication is a
// property of a live instance, and a case whose verdict depended on whether
// ONGOING_ACCESS_SECRET happened to be exported would not be a recording of
// anything.
type replayTransport struct{ dir string }

func (t replayTransport) kind() string { return transportReplay }

func (t replayTransport) get(_ context.Context, path string) ([]byte, error) {
	name := strings.TrimPrefix(strings.TrimPrefix(path, "/api"), "/")
	matches, err := filepath.Glob(filepath.Join(t.dir, name+".*.json"))
	if err != nil || len(matches) == 0 {
		// Nothing recorded for this endpoint is the recording's way of saying
		// the instance did not answer. It is source_unavailable for the same
		// reason a refused connection is: nothing came back.
		return nil, fmt.Errorf("%w: no recorded response for %s", errUnavailable, name)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("%w: %d recorded responses for %s, want exactly one",
			errUnavailable, len(matches), name)
	}

	status, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(matches[0]), name+"."), ".json"))
	if err != nil {
		return nil, fmt.Errorf("%w: recorded response %s names no HTTP status",
			errUnavailable, filepath.Base(matches[0]))
	}
	body, err := os.ReadFile(matches[0]) //nolint:gosec // the path came from a glob of the configured replay directory
	if err != nil {
		return nil, fmt.Errorf("%w: recorded response %s is unreadable",
			errUnavailable, filepath.Base(matches[0]))
	}
	if status == http.StatusOK {
		return body, nil
	}
	return nil, statusError(status)
}

// statusError maps an HTTP status to the contract's vocabulary.
//
// 401 and 403 are source_denied; anything else that is not a 200 is
// source_unavailable. Neither is ever an empty success. A denial carries no
// detail at all — not even which of the two statuses it was — because anything
// specific enough to be useful about a refused request is something the caller
// did not prove it may know. Whether this machine had a secret to offer is a
// fact about this machine, and health reports it there.
func statusError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return errDenied
	case http.StatusServiceUnavailable:
		return fmt.Errorf("%w: the instance could not open its catalog", errUnavailable)
	default:
		return fmt.Errorf("%w: the instance answered HTTP %d", errUnavailable, status)
	}
}

// unreachable renders a transport failure without repeating the address.
//
// A dial error's text carries the host and port, which are already in the
// source's configured location; what a reader needs is that nothing answered.
// A cancelled request keeps its own error so the deadline path stays
// distinguishable from a dead host.
func unreachable(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return fmt.Errorf("%w: the instance did not answer in time", errUnavailable)
	}
	return fmt.Errorf("%w: the instance could not be reached", errUnavailable)
}

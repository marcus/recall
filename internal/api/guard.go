package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"mime"
	"net/http"
	"strings"
)

// guard applies the checks that stand between a loopback socket and a browser.
//
// Binding loopback is not by itself a boundary. Any page the user visits can
// make their browser issue requests to 127.0.0.1, and a hostname that resolves
// to a loopback address is enough to make those requests same-origin as far as
// the browser is concerned — the DNS rebinding attack. This API answers queries
// over the user's own documents, tasks, and notes, so a page that could reach
// it and read the reply would have read the corpus.
//
// Three checks close that off:
//
//   - Any request carrying an Origin header is refused outright. A non-browser
//     client — curl, an agent host, the CLI dispatching to a running server —
//     sends no Origin, so refusing every request that has one costs a real
//     caller nothing and removes browsers as a class.
//   - A body-carrying request must declare JSON. A form post, which is the
//     request shape a page can issue without a preflight, cannot.
//   - No CORS headers are ever sent. Without them a browser will not hand a
//     reply back to the page that asked, even if a request slipped through.
//
// The layers overlap on purpose. Any one of them failing open should still
// leave the corpus unreadable from a web page.
func guard(r *http.Request, token string) (*Problem, int) {
	if origin := r.Header.Get("Origin"); origin != "" {
		return &Problem{CodeOriginRejected,
				"this API refuses requests carrying an Origin header; it is local-only and is not reachable from a browser"},
			http.StatusForbidden
	}
	if token != "" {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		presented := sha256.Sum256([]byte(strings.TrimPrefix(auth, prefix)))
		expected := sha256.Sum256([]byte(token))
		if !strings.HasPrefix(auth, prefix) ||
			subtle.ConstantTimeCompare(presented[:], expected[:]) != 1 {
			return &Problem{CodeUnauthorized, "a valid bearer token is required"}, http.StatusUnauthorized
		}
	}

	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
		return nil, 0
	}
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return &Problem{CodeBadRequest, "a request with a body must set Content-Type: application/json"},
			http.StatusUnsupportedMediaType
	}
	media, _, err := mime.ParseMediaType(ct)
	if err != nil || !strings.EqualFold(media, "application/json") {
		return &Problem{CodeBadRequest, "this API accepts application/json only"},
			http.StatusUnsupportedMediaType
	}
	return nil, 0
}

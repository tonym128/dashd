package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
)

// WithAuth requires a bearer token on every request except /healthz.
//
// It returns next completely untouched when token is empty, which is the
// default. That is the whole contract for the common case: a user on localhost
// should not be able to detect this handler is in the path at all.
//
// A token is worth having whenever the bind address is not loopback. The
// dashboard is read-only but not anonymous — project paths, session titles
// derived from conversation content, model names and per-session cost are all
// in it, and that is enough to tell a stranger what someone is working on and
// how long it has been taking them.
func WithAuth(token string, log *slog.Logger, next http.Handler) http.Handler {
	return WithSplitAuth(token, "", log, next)
}

// WithSplitAuth requires uiToken on dashboard routes and either ingestToken or
// uiToken on /api/ingest and /api/models. /healthz stays unauthenticated.
func WithSplitAuth(uiToken, ingestToken string, log *slog.Logger, next http.Handler) http.Handler {
	if uiToken == "" && ingestToken == "" {
		return next
	}
	if log == nil {
		log = slog.Default()
	}
	return &authHandler{token: uiToken, ingestToken: ingestToken, log: log, next: next}
}

type authHandler struct {
	token       string
	ingestToken string
	log         *slog.Logger
	next        http.Handler
}

func (a *authHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// /healthz stays open on purpose. A healthcheck is a process supervisor
	// asking whether the thing is up; making it carry a secret means the token
	// ends up in the container healthcheck command, in the compose file and in
	// every uptime monitor's config, which spreads it much further than the one
	// request it protects. It also returns only aggregate counts, so it leaks
	// nothing a scanner could not already see.
	if r.URL.Path == "/healthz" {
		a.next.ServeHTTP(w, r)
		return
	}

	isIngestRoute := r.URL.Path == "/api/ingest" || r.URL.Path == "/api/models"
	if isIngestRoute && a.token == "" && a.ingestToken == "" {
		a.next.ServeHTTP(w, r)
		return
	}
	if !isIngestRoute && a.token == "" {
		a.next.ServeHTTP(w, r)
		return
	}

	if !a.authorized(r, isIngestRoute) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="dashd"`)
		// A fixed body. Anything derived from the presented token — even
		// "token too short", which sounds helpful — turns the endpoint into an
		// oracle that reports how much of the guess was right.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		// Debug, not Warn: an unauthenticated endpoint on a public interface
		// is a thing bots find, and a log line per attempt is a way to fill
		// the disk. The 401 itself is the signal.
		a.log.Debug("rejected unauthenticated request",
			"method", r.Method, "path", r.URL.Path)
		return
	}

	a.next.ServeHTTP(w, r)
}

// authorized reports whether the request carries a matching token, from either header.
func (a *authHandler) authorized(r *http.Request, allowIngest bool) bool {
	for _, presented := range presentedTokens(r) {
		if presented == "" {
			continue
		}
		if a.token != "" && constantTimeMatch(a.token, presented) {
			return true
		}
		if allowIngest && a.ingestToken != "" && constantTimeMatch(a.ingestToken, presented) {
			return true
		}
	}
	return false
}

func constantTimeMatch(expected, presented string) bool {
	want := sha256.Sum256([]byte(expected))
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// presentedTokens returns the candidate tokens on the request, in no
// particular order; the caller accepts any match.
func presentedTokens(r *http.Request) []string {
	var out []string
	if h := r.Header.Get("Authorization"); h != "" {
		// RFC 7235 says the scheme is case-insensitive.
		if len(h) >= len("bearer ") && strings.EqualFold(h[:len("bearer ")], "bearer ") {
			out = append(out, strings.TrimSpace(h[len("bearer "):]))
		}
	}
	// Accepted because a browser cannot set a header on a plain navigation.
	// Without it, -auth-token would leave the dashboard unusable from the URL
	// bar, which is the only place a user can reach it.
	return append(out, strings.TrimSpace(r.Header.Get("X-Auth-Token")))
}

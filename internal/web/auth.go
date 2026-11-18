package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Role is what an authenticated request is allowed to do. Roles are ordered:
// each one includes everything below it, except that RoleIngest grants no read
// access to the dashboard at all.
type Role int

const (
	RoleNone Role = iota
	// RoleIngest may POST /api/ingest and GET /api/models, nothing else. It is
	// what a push agent on a laptop should hold, so a stolen laptop exposes the
	// ability to add data, not to read everyone's.
	RoleIngest
	// RoleUI may read the dashboard and its JSON APIs, and ingest.
	RoleUI
	// RoleAdmin may additionally run state-changing maintenance: reprice,
	// prune, forget a session, rename a machine.
	RoleAdmin
)

// AuthConfig configures WithAuthConfig.
type AuthConfig struct {
	// UIToken protects the dashboard. Empty leaves the dashboard open.
	UIToken string
	// IngestToken is a write-only token for /api/ingest and /api/models.
	IngestToken string
	// AdminToken, when set, is required for admin routes. When empty the UI
	// token (if any) also grants admin.
	AdminToken string
	// MachineTokens maps a write-only ingest token to the one machine it may
	// push as. A request authenticated this way has its machine forced to the
	// bound name, so one laptop cannot overwrite another's sessions.
	MachineTokens map[string]string
	// TrustProxy uses the first X-Forwarded-For address as the client IP for
	// rate limiting and logging. Only enable behind a proxy you control.
	TrustProxy bool
	Log        *slog.Logger
}

func (c AuthConfig) any() bool {
	return c.UIToken != "" || c.IngestToken != "" || c.AdminToken != "" || len(c.MachineTokens) > 0
}

// WithAuth requires a bearer token on every request except /healthz.
//
// It returns next completely untouched when token is empty, which is the
// default. That is the whole contract for the common case: a user on localhost
// should not be able to detect this handler is in the path at all.
func WithAuth(token string, log *slog.Logger, next http.Handler) http.Handler {
	return WithAuthConfig(AuthConfig{UIToken: token, Log: log}, next)
}

// WithSplitAuth requires uiToken on dashboard routes and either ingestToken or
// uiToken on /api/ingest and /api/models. /healthz stays unauthenticated.
func WithSplitAuth(uiToken, ingestToken string, log *slog.Logger, next http.Handler) http.Handler {
	return WithAuthConfig(AuthConfig{UIToken: uiToken, IngestToken: ingestToken, Log: log}, next)
}

// WithAuthConfig wraps next with token, cookie and role checks.
func WithAuthConfig(cfg AuthConfig, next http.Handler) http.Handler {
	if !cfg.any() {
		return next
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &authHandler{
		cfg:          cfg,
		next:         next,
		loginLimiter: rateLimiter{rate: 0.2, burst: 10},
		failLog:      throttledLog{every: time.Minute},
	}
}

type authHandler struct {
	cfg          AuthConfig
	next         http.Handler
	loginLimiter rateLimiter
	failLog      throttledLog
}

// routeRole is the minimum role a path needs.
func routeRole(path string) Role {
	switch {
	case path == "/api/ingest" || path == "/api/models":
		return RoleIngest
	case isAdminPath(path):
		return RoleAdmin
	default:
		return RoleUI
	}
}

func isAdminPath(path string) bool {
	return path == "/api/reprice" || strings.HasPrefix(path, "/api/admin/")
}

// required reports the role a route needs given what is configured. A route
// whose protecting token is not configured is open, which keeps the historical
// behaviour: -ingest-token alone protects ingest and leaves the UI open.
func (a *authHandler) required(path string) Role {
	need := routeRole(path)
	switch need {
	case RoleIngest:
		if a.cfg.UIToken == "" && a.cfg.IngestToken == "" && a.cfg.AdminToken == "" && len(a.cfg.MachineTokens) == 0 {
			return RoleNone
		}
	case RoleUI:
		if a.cfg.UIToken == "" && a.cfg.AdminToken == "" {
			return RoleNone
		}
	case RoleAdmin:
		if a.cfg.UIToken == "" && a.cfg.AdminToken == "" {
			// No token protects admin routes; the server's loopback rule
			// (see adminAllowed) decides instead.
			return RoleNone
		}
	}
	return need
}

func (a *authHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// /healthz stays open on purpose. A healthcheck is a process supervisor
	// asking whether the thing is up; making it carry a secret means the token
	// ends up in the container healthcheck command, in the compose file and in
	// every uptime monitor's config, which spreads it much further than the one
	// request it protects. It also returns only aggregate counts.
	switch r.URL.Path {
	case "/healthz":
		a.next.ServeHTTP(w, r)
		return
	case "/login":
		a.serveLogin(w, r)
		return
	case "/logout":
		a.serveLogout(w, r)
		return
	}

	need := a.required(r.URL.Path)
	role, machine, viaCookie := a.authenticate(r)
	if need == RoleNone {
		a.next.ServeHTTP(w, withAuthContext(r, maxRole(role, RoleNone), machine, true))
		return
	}

	if role < need {
		a.reject(w, r, need)
		return
	}
	// A cookie rides along on every request the browser makes, including
	// ones a hostile page triggers, so a state-changing request authenticated
	// by cookie must also carry a header a cross-site form cannot set.
	if viaCookie && r.Method != http.MethodGet && r.Method != http.MethodHead &&
		r.Header.Get("X-Dashd-Request") == "" {
		http.Error(w, "missing X-Dashd-Request header", http.StatusForbidden)
		return
	}
	a.next.ServeHTTP(w, withAuthContext(r, role, machine, false))
}

func maxRole(a, b Role) Role {
	if a > b {
		return a
	}
	return b
}

func (a *authHandler) reject(w http.ResponseWriter, r *http.Request, need Role) {
	ip := clientIP(r, a.cfg.TrustProxy)
	if a.failLog.allow(ip, time.Now()) {
		// Throttled to one line per address per minute: an endpoint on a
		// public interface is a thing bots find, and a line per attempt is a
		// way to fill the disk. Once a minute is enough to see it happening.
		a.cfg.Log.Warn("rejected unauthenticated request",
			"ip", ip, "method", r.Method, "path", r.URL.Path)
	}
	// A browser navigating to the dashboard gets the login form instead of a
	// bare 401, which is the only way a token-protected dashboard is usable
	// without a header-injecting proxy.
	if need >= RoleUI && r.Method == http.MethodGet && acceptsHTML(r) {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="dashd"`)
	// A fixed body. Anything derived from the presented token — even
	// "token too short", which sounds helpful — turns the endpoint into an
	// oracle that reports how much of the guess was right.
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// authenticate returns the strongest role the request proves, the machine a
// machine-bound token pins it to, and whether the proof was a cookie.
func (a *authHandler) authenticate(r *http.Request) (Role, string, bool) {
	if tok := bearerToken(r); tok != "" {
		role, machine := a.roleForToken(tok)
		if role > RoleNone {
			return role, machine, false
		}
	}
	if c, err := r.Cookie(cookieName); err == nil {
		if role := a.roleForCookie(c.Value); role > RoleNone {
			return role, "", true
		}
	}
	return RoleNone, "", false
}

func (a *authHandler) roleForToken(tok string) (Role, string) {
	if a.cfg.AdminToken != "" && constantTimeMatch(a.cfg.AdminToken, tok) {
		return RoleAdmin, ""
	}
	if a.cfg.UIToken != "" && constantTimeMatch(a.cfg.UIToken, tok) {
		if a.cfg.AdminToken == "" {
			return RoleAdmin, ""
		}
		return RoleUI, ""
	}
	if a.cfg.IngestToken != "" && constantTimeMatch(a.cfg.IngestToken, tok) {
		return RoleIngest, ""
	}
	// Every machine token is compared, matched or not, so the time taken
	// does not say which entry (if any) was close.
	matched := ""
	for t, m := range a.cfg.MachineTokens {
		if t != "" && constantTimeMatch(t, tok) {
			matched = m
		}
	}
	if matched != "" {
		return RoleIngest, matched
	}
	return RoleNone, ""
}

// ---------------------------------------------------------------- cookies

const cookieName = "dashd_auth"

// cookieValue is "<role>.<mac>", where the MAC is keyed by the token that
// grants the role. The server keeps no session state: rotating the token
// invalidates every cookie minted from it, which is what rotation should do.
func cookieValue(role Role, key string) string {
	name := "ui"
	if role == RoleAdmin {
		name = "admin"
	}
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte("dashd-cookie-v1:" + name))
	return name + "." + hex.EncodeToString(m.Sum(nil))
}

func (a *authHandler) roleForCookie(v string) Role {
	name, _, ok := strings.Cut(v, ".")
	if !ok {
		return RoleNone
	}
	switch name {
	case "admin":
		key := a.cfg.AdminToken
		if key == "" {
			key = a.cfg.UIToken
		}
		if key != "" && hmac.Equal([]byte(v), []byte(cookieValue(RoleAdmin, key))) {
			return RoleAdmin
		}
	case "ui":
		if a.cfg.UIToken != "" && hmac.Equal([]byte(v), []byte(cookieValue(RoleUI, a.cfg.UIToken))) {
			return RoleUI
		}
	}
	return RoleNone
}

func isSecureRequest(r *http.Request, trustProxy bool) bool {
	if r.TLS != nil {
		return true
	}
	return trustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

var loginTmpl = template.Must(template.New("login").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Sign in · dashd</title>
<style>
:root{color-scheme:light dark;--bg:#0f1115;--card:#171a21;--text:#e6e8ee;--muted:#9aa3b2;--accent:#5b8def;--err:#f87171}
@media (prefers-color-scheme: light){:root{--bg:#f5f6f8;--card:#fff;--text:#14171f;--muted:#5b6475}}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--text);font:15px/1.5 system-ui,sans-serif;padding:16px}
form{background:var(--card);padding:28px;border-radius:12px;width:100%;max-width:360px;box-shadow:0 4px 24px rgba(0,0,0,.2)}
h1{font-size:20px;margin:0 0 4px}p{color:var(--muted);margin:0 0 18px;font-size:14px}
label{display:block;font-size:13px;margin-bottom:6px}
input{box-sizing:border-box;width:100%;padding:10px 12px;border-radius:8px;border:1px solid #3a4150;background:transparent;color:inherit;font:inherit}
button{margin-top:16px;width:100%;padding:10px;border:0;border-radius:8px;background:var(--accent);color:#fff;font:inherit;cursor:pointer}
.err{color:var(--err);font-size:14px;margin-bottom:12px}
</style></head><body>
<form method="post" action="/login">
<h1>dashd</h1><p>Enter the dashboard token to continue.</p>
{{if .Error}}<div class="err" role="alert">{{.Error}}</div>{{end}}
<label for="token">Token</label>
<input id="token" name="token" type="password" autocomplete="current-password" required autofocus>
<input type="hidden" name="next" value="{{.Next}}">
<button type="submit">Sign in</button>
</form></body></html>`))

func (a *authHandler) serveLogin(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")

	next := safeNext(r.FormValue("next"))
	render := func(code int, msg string) {
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(code)
		_ = loginTmpl.Execute(w, map[string]string{"Error": msg, "Next": next})
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		render(http.StatusOK, "")
	case http.MethodPost:
		if !sameOrigin(r) {
			http.Error(w, "cross-origin login refused", http.StatusForbidden)
			return
		}
		ip := clientIP(r, a.cfg.TrustProxy)
		if !a.loginLimiter.allow(ip, time.Now()) {
			render(http.StatusTooManyRequests, "Too many attempts. Wait a minute and try again.")
			return
		}
		role, _ := a.roleForToken(strings.TrimSpace(r.PostFormValue("token")))
		if role < RoleUI {
			if a.failLog.allow("login:"+ip, time.Now()) {
				a.cfg.Log.Warn("failed dashboard login", "ip", ip)
			}
			render(http.StatusUnauthorized, "That token is not valid.")
			return
		}
		key := a.cfg.UIToken
		if role == RoleAdmin && a.cfg.AdminToken != "" {
			key = a.cfg.AdminToken
		}
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    cookieValue(role, key),
			Path:     "/",
			MaxAge:   30 * 24 * 3600,
			HttpOnly: true,
			Secure:   isSecureRequest(r, a.cfg.TrustProxy),
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, next, http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *authHandler) serveLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin logout refused", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isSecureRequest(r, a.cfg.TrustProxy)})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// safeNext keeps the post-login redirect on this origin: a path, never a
// scheme-relative or absolute URL.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// ---------------------------------------------------------------- context

type authCtxKey struct{}

type authInfo struct {
	role    Role
	machine string
	// open is true when no token protected the route, so loopback rules apply.
	open bool
}

func withAuthContext(r *http.Request, role Role, machine string, open bool) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authCtxKey{}, authInfo{role: role, machine: machine, open: open}))
}

// BoundMachine returns the machine a machine-bound ingest token pins this
// request to, or "".
func BoundMachine(ctx context.Context) string {
	info, _ := ctx.Value(authCtxKey{}).(authInfo)
	return info.machine
}

// adminAllowed reports whether r may run an admin action. With a token
// configured the auth middleware has already enforced RoleAdmin. Without one,
// only a request from this machine may: an open dashboard on loopback is
// single-user by construction, and that is the only case where "no token"
// can safely mean "allowed to change data".
func adminAllowed(r *http.Request) bool {
	info, ok := r.Context().Value(authCtxKey{}).(authInfo)
	if ok && !info.open {
		return info.role >= RoleAdmin
	}
	return isLoopbackAddr(r.RemoteAddr)
}

func isLoopbackAddr(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------- helpers

func constantTimeMatch(expected, presented string) bool {
	want := sha256.Sum256([]byte(expected))
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// bearerToken returns the token in an Authorization: Bearer header, or "".
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	// RFC 7235 says the scheme is case-insensitive.
	if len(h) >= len("bearer ") && strings.EqualFold(h[:len("bearer ")], "bearer ") {
		return strings.TrimSpace(h[len("bearer "):])
	}
	return ""
}

// clientIP is the address used for rate limiting and logs.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// throttledLog allows one event per key per interval.
type throttledLog struct {
	every time.Duration
	mu    sync.Mutex
	last  map[string]time.Time
}

func (t *throttledLog) allow(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	if len(t.last) > 4096 {
		for k, v := range t.last {
			if now.Sub(v) > t.every {
				delete(t.last, k)
			}
		}
	}
	if prev, ok := t.last[key]; ok && now.Sub(prev) < t.every {
		return false
	}
	t.last[key] = now
	return true
}

package web

import (
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Cross-site and DNS-rebinding defences.
//
// The default deployment is loopback with no token, and "nothing off-host can
// reach it" is not the same as "nothing can reach it": every web page the user
// opens runs in a browser on the same machine. Two attacks follow from that.
//
//   - Cross-site writes. A page can POST to http://127.0.0.1:8753/api/ingest
//     with `mode: "no-cors"` and a text/plain body. The browser sends it; the
//     page just cannot read the answer. withRequestGuards refuses any
//     state-changing request whose Origin or Sec-Fetch-Site says it came from
//     another site, and requires application/json on ingest, which a
//     cross-site "simple" request cannot send without a CORS preflight that
//     this server never answers.
//   - DNS rebinding. A hostile name that first resolves to the attacker and
//     then to 127.0.0.1 makes the dashboard same-origin with the attacker's
//     page, so it can read everything. The page's requests still carry the
//     hostile name in Host, which is what WithHostCheck refuses.

// withRequestGuards is applied inside Server.Handler, so it protects every
// deployment including tests.
func withRequestGuards(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !sameOrigin(r) {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
			if r.URL.Path == "/api/ingest" && !isJSON(r.Header.Get("Content-Type")) {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isJSON(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}

// sameOrigin reports whether a request was not sent cross-site. A request with
// neither header is a non-browser client (curl, dashd push) and is allowed;
// browsers always send Origin on a cross-origin POST.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		if site != "same-origin" && site != "none" {
			return false
		}
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// WithHostCheck refuses requests whose Host header is not this server's. An IP
// literal, localhost and *.localhost are always accepted — a rebinding attack
// needs a hostname, by definition — plus any name in allowed. "*" in allowed
// disables the check.
func WithHostCheck(allowed []string, next http.Handler) http.Handler {
	set := map[string]bool{}
	for _, h := range allowed {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "*" {
			return next
		}
		if h != "" {
			set[h] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || hostAllowed(r.Host, set) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unrecognised Host header; start dashd with -allowed-hosts to permit it", http.StatusMisdirectedRequest)
	})
}

func hostAllowed(hostport string, set map[string]bool) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	return set[host]
}

// ---------------------------------------------------------------- rate limiting

type bucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter is a per-key token bucket.
type rateLimiter struct {
	rate  float64 // tokens per second
	burst float64

	mu      sync.Mutex
	buckets map[string]*bucket
}

func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = make(map[string]*bucket)
	}
	// Evict stale entries occasionally to keep memory bounded.
	if len(l.buckets) > 1024 {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[key]
	if !ok {
		l.buckets[key] = &bucket{tokens: l.burst - 1, last: now}
		return true
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

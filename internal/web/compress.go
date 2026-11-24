package web

import (
	"compress/gzip"
	"net/http"
	"strings"
)

// withGzip compresses text responses for clients that accept it. The page is
// mostly repetitive JSON, so this is worth about 85% of its size.
func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		q := strings.ReplaceAll(strings.TrimSpace(params), " ", "")
		return q != "q=0" && !strings.HasPrefix(q, "q=0.000") && q != "q=0.0" && q != "q=0.00"
	}
	return false
}

type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	started bool
}

// start decides, once, from the headers the handler set.
func (g *gzipWriter) start(code int) {
	if g.started {
		return
	}
	g.started = true
	h := g.Header()
	ct := h.Get("Content-Type")
	text := strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "javascript")
	if text {
		h.Add("Vary", "Accept-Encoding")
	}
	if !text || h.Get("Content-Encoding") != "" || code < 200 || code == http.StatusNoContent || code == http.StatusNotModified {
		return
	}
	h.Set("Content-Encoding", "gzip")
	h.Del("Content-Length")
	// The bytes differ from the identity response, so a strong validator must not
	// cover both.
	if e := h.Get("ETag"); e != "" && !strings.HasPrefix(e, "W/") {
		h.Set("ETag", "W/"+e)
	}
	g.gz = gzip.NewWriter(g.ResponseWriter)
}

func (g *gzipWriter) WriteHeader(code int) {
	if code < 200 {
		g.ResponseWriter.WriteHeader(code)
		return
	}
	g.start(code)
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.started {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.start(http.StatusOK)
	}
	if g.gz != nil {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipWriter) Flush() {
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection, for deadlines.
func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) close() {
	if g.gz != nil {
		_ = g.gz.Close()
	}
}

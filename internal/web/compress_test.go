package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesAreCompressedOnRequest(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	do := func(path string, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for i := 0; i < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/", "/assets/dashboard.js", "/healthz", "/api/sessions"} {
		plain := do(path)
		if plain.Header().Get("Content-Encoding") != "" {
			t.Errorf("%s compressed without being asked", path)
		}
		zipped := do(path, "Accept-Encoding", "br, gzip;q=0.8")
		if zipped.Header().Get("Content-Encoding") != "gzip" {
			t.Errorf("%s not compressed", path)
			continue
		}
		zr, err := gzip.NewReader(zipped.Body)
		if err != nil {
			t.Fatal(err)
		}
		if body, err := io.ReadAll(zr); err != nil || len(body) == 0 {
			t.Errorf("%s: %d bytes, %v", path, len(body), err)
		}
	}
	if do("/", "Accept-Encoding", "gzip;q=0").Header().Get("Content-Encoding") != "" {
		t.Error("gzip;q=0 was ignored")
	}
}

func TestAssetsRevalidateWithAnETag(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	for _, enc := range []string{"", "gzip"} {
		req := httptest.NewRequest(http.MethodGet, "/assets/dashboard.css", nil)
		req.Header.Set("Accept-Encoding", enc)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		tag := rec.Header().Get("ETag")
		if !strings.Contains(tag, `"`) {
			t.Fatalf("no ETag: %q", tag)
		}
		req.Header.Set("If-None-Match", tag)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Errorf("enc %q: status %d with %d bytes, want an empty 304", enc, rec.Code, rec.Body.Len())
		}
	}
}

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonym128/dashd/internal/model"
)

func TestCheckExposure(t *testing.T) {
	cases := []struct {
		o    options
		fail bool
	}{
		{options{addr: "127.0.0.1:8753"}, false},
		{options{addr: "0.0.0.0:8753"}, true},
		{options{addr: "0.0.0.0:8753", authToken: "x"}, false},
		{options{addr: ":8753", insecureNoAuth: true}, false},
		{options{addr: "127.0.0.1:1", tlsCert: "c"}, true},
	}
	for _, tc := range cases {
		if err := checkExposure(tc.o); (err != nil) != tc.fail {
			t.Errorf("checkExposure(%+v) = %v, want fail=%v", tc.o, err, tc.fail)
		}
	}
}

func TestIngestEnabledDefaults(t *testing.T) {
	if ingestEnabled(options{}, "serve-and-scan") {
		t.Error("serve-and-scan accepts pushes by default")
	}
	if !ingestEnabled(options{}, "serve") || !ingestEnabled(options{acceptIngest: true}, "serve-and-scan") ||
		!ingestEnabled(options{ingestToken: "t"}, "serve-and-scan") {
		t.Error("ingest should be on for serve, -accept-ingest or an ingest token")
	}
}

func TestQueueMergesWritesForOneSession(t *testing.T) {
	r := newRemoteSink("http://x", "", "m")
	mk := func(keys ...string) model.SessionWrite {
		s := model.SessionWrite{Session: model.Session{UID: "u"}}
		for _, k := range keys {
			s.Calls = append(s.Calls, model.Call{CallKey: k})
		}
		return s
	}
	_ = r.ReplaceSession(mk("a"))
	_ = r.AppendSession(mk("b"))
	_ = r.AppendSession(mk("c"))
	it := r.pending["u"]
	if it.Mode != "replace" || len(it.Session.Calls) != 3 || len(r.order) != 1 {
		t.Fatalf("merged = %s with %d calls, order %v", it.Mode, len(it.Session.Calls), r.order)
	}
	_ = r.ReplaceSession(mk("z"))
	if got := r.pending["u"]; len(got.Session.Calls) != 1 {
		t.Fatal("a replace did not supersede the buffered writes")
	}
}

func TestRedaction(t *testing.T) {
	r := newRemoteSink("http://x", "", "m")
	r.home = "/home/u"
	sess := model.SessionWrite{Session: model.Session{UID: "u", Project: "/home/u/src/app", Title: "secret plan"},
		Path: "/home/u/.claude/x.jsonl", Calls: []model.Call{{Project: "/home/u/src/app"}}}
	for mode, want := range map[redactMode]string{redactHome: "~/src/app", redactBasename: "app"} {
		r.redact, r.redactTitles = mode, true
		got := r.redacted(sess)
		if got.Project != want || got.Calls[0].Project != want || got.Title != "" {
			t.Errorf("mode %d: %+v", mode, got)
		}
	}
	r.redact = redactHash
	if got := r.redacted(sess).Project; !strings.HasPrefix(got, "h:") || strings.Contains(got, "app") {
		t.Errorf("hash = %q", got)
	}
	if sess.Project != "/home/u/src/app" {
		t.Error("redaction mutated the caller's session")
	}
	if _, err := parseRedactMode("bogus"); err == nil {
		t.Error("bad redact mode accepted")
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
		ok   bool
	}{
		{"0.6.1", "0.10.0", -1, true},
		{"v1.2.0", "1.2", 0, true},
		{"1.3.0-rc1", "1.2.9", 1, true},
		{"dev", "1.0.0", 0, false},
	} {
		got, ok := compareVersions(tc.a, tc.b)
		if got != tc.want || ok != tc.ok {
			t.Errorf("compareVersions(%q,%q) = %d,%v", tc.a, tc.b, got, ok)
		}
	}
}

func TestSelfUpdateRejectsChecksumMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v0.9.0"}`))
		case "/dl/v0.9.0/dashd_0.9.0_linux_amd64.tar.gz":
			_, _ = w.Write([]byte("tampered"))
		case "/dl/v0.9.0/checksums.txt":
			_, _ = w.Write([]byte("0000  dashd_0.9.0_linux_amd64.tar.gz\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "dashd")
	_ = os.WriteFile(exe, []byte("old"), 0o755)
	err := selfUpdateFrom(context.Background(), srv.URL+"/latest", srv.URL+"/dl", "0.8.0", "linux", "amd64", exe, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Fatal("binary replaced despite the mismatch")
	}
	// Never downgrade.
	var out bytes.Buffer
	if err := selfUpdateFrom(context.Background(), srv.URL+"/latest", srv.URL+"/dl", "1.0.0", "linux", "amd64", exe, &out); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMachineTokensAndSecrets(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tokens")
	_ = os.WriteFile(p, []byte("# comment\nlaptop  aaaaaaaaaaaaaaaaaaaa\ndesk bbbbbbbbbbbbbbbbbbbb\n"), 0o600)
	m, err := loadMachineTokens(p)
	if err != nil || m["aaaaaaaaaaaaaaaaaaaa"] != "laptop" || len(m) != 2 {
		t.Fatalf("tokens = %v, %v", m, err)
	}
	_ = os.WriteFile(p, []byte("laptop short\n"), 0o600)
	if _, err := loadMachineTokens(p); err == nil {
		t.Fatal("short token accepted")
	}
	tf := filepath.Join(dir, "ui")
	_ = os.WriteFile(tf, []byte("  file-token \n"), 0o600)
	o := options{authTokenFile: tf}
	t.Setenv("DASHD_AUTH_TOKEN", "env-token")
	if err := resolveSecrets(&o); err != nil || o.authToken != "file-token" {
		t.Fatalf("authToken = %q, %v; the file should beat the environment", o.authToken, err)
	}
}

func TestCSVSafe(t *testing.T) {
	if csvSafe("=HYPERLINK(1)") != "'=HYPERLINK(1)" || csvSafe("/p") != "/p" {
		t.Fatal("csvSafe")
	}
}

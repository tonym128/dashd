package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/store"
	"github.com/tonym128/dashd/internal/web"
)

// seededDB writes a small database with two sessions and returns its path.
func seededDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "d.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, m := range []string{"claude-sonnet-4-6", "unknown-x"} {
		uid := "s" + string(rune('a'+i))
		_ = st.ReplaceSession(model.SessionWrite{
			Session: model.Session{UID: uid, Agent: "claude", Project: "/p/" + uid, Machine: "box"},
			Calls: []model.Call{{SessionUID: uid, Agent: "claude", Model: m, CallKey: "1",
				Time: now.Add(-time.Duration(i) * time.Hour), InputTokens: 100, OutputTokens: 10,
				TotalTokens: 110, CostUSD: float64(i + 1), Priced: i == 0}},
		})
		_ = st.RecomputeSession(uid)
	}
	st.Close()
	return path
}

func cli(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runCLI(args, &out, &errOut)
	return out.String() + errOut.String(), code
}

func TestCLIDataCommands(t *testing.T) {
	db := seededDB(t)
	dir := t.TempDir()

	if out, code := cli(t, "stats", "-db", db, "-models", "/nonexistent.json"); code != 0 || !strings.Contains(out, "sessions") {
		t.Fatalf("stats = %d %s", code, out)
	}
	if out, code := cli(t, "insights", "-db", db, "-json", "-project", "/p/sa", "-since", "2000-01-01", "-until", "2100-01-01"); code != 0 || !strings.HasPrefix(strings.TrimSpace(out[strings.Index(out, "["):]), "[") {
		t.Fatalf("insights -json = %d %s", code, out)
	}
	if out, code := cli(t, "insights", "-db", db); code != 0 || out == "" {
		t.Fatalf("insights = %d %s", code, out)
	}
	if out, code := cli(t, "insights", "-db", db, "-since", "nope"); code == 0 {
		t.Fatalf("bad -since accepted: %s", out)
	}

	csvPath := filepath.Join(dir, "calls.csv")
	if out, code := cli(t, "export", "-db", db, "-out", csvPath); code != 0 {
		t.Fatalf("export = %d %s", code, out)
	}
	if data, _ := os.ReadFile(csvPath); strings.Count(string(data), "\n") != 3 {
		t.Fatalf("csv export:\n%s", data)
	}
	jsonPath := filepath.Join(dir, "calls.json")
	if out, code := cli(t, "export", "-db", db, "-format", "json", "-out", jsonPath, "-model", "unknown-x"); code != 0 {
		t.Fatalf("json export = %d %s", code, out)
	}
	var calls []map[string]any
	data, _ := os.ReadFile(jsonPath)
	if err := json.Unmarshal(data, &calls); err != nil || len(calls) != 1 {
		t.Fatalf("json export = %v %s", err, data)
	}
	if _, code := cli(t, "export", "-db", db, "-format", "xml"); code == 0 {
		t.Fatal("unknown export format accepted")
	}

	if out, code := cli(t, "digest", "-db", db, "-digest-every", "48h"); code != 0 || !strings.Contains(out, "dashd digest") {
		t.Fatalf("digest = %d %s", code, out)
	}
	var hooked string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hooked = string(b)
	}))
	defer hook.Close()
	if out, code := cli(t, "digest", "-db", db, "-webhook-url", hook.URL); code != 0 || !strings.Contains(out, "digest sent") || !strings.Contains(hooked, "digest") {
		t.Fatalf("digest webhook = %d %s / %s", code, out, hooked)
	}

	bak := filepath.Join(dir, "copy.db")
	if out, code := cli(t, "backup", "-db", db, "-out", bak); code != 0 || !strings.Contains(out, bak) {
		t.Fatalf("backup = %d %s", code, out)
	}
	if out, code := cli(t, "prune", "-db", db, "-before", "2100-01-01", "-vacuum=false"); code != 0 || !strings.Contains(out, "pruned 2 calls") {
		t.Fatalf("prune = %d %s", code, out)
	}
	if _, code := cli(t, "prune", "-db", db); code == 0 {
		t.Fatal("prune without -before accepted")
	}
}

func TestCLIRefusesExposedServeAndBadTZ(t *testing.T) {
	db := seededDB(t)
	if out, code := cli(t, "serve", "-db", db, "-addr", "0.0.0.0:0"); code == 0 || !strings.Contains(out, "refusing") {
		t.Fatalf("exposed serve = %d %s", code, out)
	}
	if out, code := cli(t, "stats", "-db", db, "-tz", "Not/AZone"); code == 0 || !strings.Contains(out, "-tz") {
		t.Fatalf("bad tz = %d %s", code, out)
	}
	tf := filepath.Join(t.TempDir(), "empty")
	_ = os.WriteFile(tf, []byte("  \n"), 0o600)
	if out, code := cli(t, "stats", "-db", db, "-auth-token-file", tf); code == 0 || !strings.Contains(out, "empty") {
		t.Fatalf("empty token file = %d %s", code, out)
	}
}

func TestStatusCommand(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer health.Close()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetMeta(metaRemote, health.URL)
	_ = st.SetMeta(metaServerVersion, "9.9.9")
	_ = st.SetMeta(metaLastOKAt, "1700000000")
	_ = st.SetMeta(metaLastError, "boom")
	_ = st.SetMeta(metaLastErrorAt, "1700000001")
	var out bytes.Buffer
	if err := runStatus(context.Background(), options{db: path, machine: "m"}, st, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"reachable", "9.9.9", "boom", "2023-"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := runStatus(context.Background(), options{db: path, jsonOut: true}, st, &out); err != nil {
		t.Fatal(err)
	}
	var s pushStatus
	if err := json.Unmarshal(out.Bytes(), &s); err != nil || s.LastError != "boom" || s.ServerHealthy == nil || !*s.ServerHealthy {
		t.Fatalf("status json = %s %v", out.String(), err)
	}
	st.Close()
	if unixMeta("x") != "" || unixMeta("0") != "" {
		t.Error("unixMeta accepted junk")
	}
}

func TestRemoteSinkResyncsWhenServerChanges(t *testing.T) {
	srvStore, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer srvStore.Close()
	srv, _ := web.New(srvStore, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	state, _ := store.Open(filepath.Join(t.TempDir(), "state.db"))
	defer state.Close()
	_ = state.SaveScanState(model.ScanState{Path: "/x.jsonl", Agent: "claude", Offset: 10})

	sink := newRemoteSink(ts.URL, "", "m").withState(state).withRedaction(redactNone, false)
	if sink.BatchSize() != maxBatchSessions || sink.Pending() != 0 {
		t.Fatal("batch accounting")
	}
	_ = sink.MarkOrphaned([]string{"gone"})
	if err := sink.Flush(); err != nil {
		t.Fatal(err)
	}
	// First contact with existing cursors built against an unknown server.
	if !sink.instanceChanged {
		t.Fatal("existing cursors were trusted against an unknown server")
	}
	if err := sink.resetCursors("test"); err != nil {
		t.Fatal(err)
	}
	if n, _ := state.CountScanStates(); n != 0 {
		t.Fatalf("%d cursors survived a reset", n)
	}
	if id, _ := state.Meta(metaInstanceID); id == "" {
		t.Fatal("instance id not recorded")
	}
	sink.recordOutcome(nil)
	if v, _ := state.Meta(metaLastOKAt); v == "" {
		t.Fatal("success not recorded")
	}
	sink.recordOutcome(io.EOF)
	if v, _ := state.Meta(metaLastError); v != "EOF" {
		t.Fatalf("error not recorded: %q", v)
	}
}

func TestSmallHelpers(t *testing.T) {
	if got := splitList(" a, ,b,"); len(got) != 2 || got[1] != "b" {
		t.Fatalf("splitList = %v", got)
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	warnInsecureRemote("https://x", log)
	warnInsecureRemote("http://127.0.0.1:1", log)
	warnInsecureRemote("http://localhost", log)
	if buf.Len() != 0 {
		t.Fatalf("warned for a safe remote: %s", buf.String())
	}
	warnInsecureRemote("http://dash.lan", log)
	if !strings.Contains(buf.String(), "plain HTTP") {
		t.Fatal("no warning for plain HTTP")
	}
	f := filepath.Join(t.TempDir(), "x")
	_ = os.WriteFile(f, []byte("abc"), 0o600)
	if fileDigest(f) == "" || fileDigest(f+"missing") != "" {
		t.Fatal("fileDigest")
	}
	if _, err := filterFromOptions(options{until: "bad"}); err == nil {
		t.Fatal("bad -until accepted")
	}
}

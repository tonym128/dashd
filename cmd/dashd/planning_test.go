package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/source"
	"github.com/tonym128/dashd/internal/store"
	"github.com/tonym128/dashd/internal/web"
)

func tempStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "me@example.com"}, {"config", "user.name", "me"},
		{"config", "commit.gpgsign", "false"}, {"commit", "-q", "--allow-empty", "-m", "secret subject"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	return dir
}

func TestPushSendsCommitsOnlyWhenAskedAndRedacts(t *testing.T) {
	dir := gitRepo(t)
	var got []model.IngestRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var req model.IngestRequest
		_ = json.NewDecoder(gz).Decode(&req)
		got = append(got, req)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	sess := model.SessionWrite{Session: model.Session{UID: "u", Project: dir}, RateLimits: []model.RateLimit{{Time: time.Now(), Detail: "text"}}}

	off := newRemoteSink(srv.URL, "", "box").withState(tempStore(t))
	_ = off.ReplaceSession(sess)
	if err := off.sendCommits(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("sent %d requests without -git (%v)", len(got), err)
	}

	on := newRemoteSink(srv.URL, "", "box").withState(tempStore(t)).withRedaction(redactHash, true)
	on.git = true
	_ = on.ReplaceSession(sess)
	_ = on.AppendSession(sess)
	if err := on.sendCommits(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Commits) != 1 || got[0].Commits[0].Subject != "" || got[0].Commits[0].Project == dir {
		t.Fatalf("%+v", got)
	}
	if err := on.sendCommits(context.Background()); err != nil || len(got) != 1 {
		t.Error("sent again before the interval")
	}
	if l := on.pending["u"].Session.RateLimits; len(l) == 0 || l[0].Detail != "" {
		t.Errorf("rate-limit text not redacted: %+v", l)
	}
}

func TestPriceAdminRepricesWhatItTouches(t *testing.T) {
	st := tempStore(t)
	if err := st.ReplaceSession(model.SessionWrite{Session: model.Session{UID: "u", Agent: "pi"}, Calls: []model.Call{
		{SessionUID: "u", CallKey: "k", Agent: "pi", Model: "mystery", Time: time.Now().Add(-time.Hour), InputTokens: 1e6}}}); err != nil {
		t.Fatal(err)
	}
	p, err := source.NewFallbackPricer()
	if err != nil {
		t.Fatal(err)
	}
	a := priceAdmin{st, p}
	ctx := context.Background()
	if n, err := a.SetPrice(ctx, "Mystery", source.Rates{Input: 4}, true); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	var cost float64
	_ = st.DB().QueryRow(`SELECT cost_usd FROM call`).Scan(&cost)
	if cost != 4 {
		t.Errorf("cost %v, want 4", cost)
	}
	if _, err := a.RemovePrice(ctx, "mystery"); err != nil {
		t.Fatal(err)
	}
	if pts, _ := st.PriceHistory(ctx); len(pts) != 0 {
		t.Errorf("%+v", pts)
	}
}

func TestBackgroundLoopsStop(t *testing.T) {
	st := tempStore(t)
	dir := gitRepo(t)
	if err := st.ReplaceSession(model.SessionWrite{Session: model.Session{UID: "u", Agent: "pi", Project: dir}, Calls: []model.Call{
		{SessionUID: "u", CallKey: "k", Agent: "pi", Project: dir, Model: "m", Time: time.Now().Add(-time.Hour), CostUSD: 1, Priced: true}}}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out := t.TempDir()
	done := make(chan struct{}, 2)
	go func() { runGitCollector(ctx, st, "box", log); done <- struct{}{} }()
	go func() { runBackups(ctx, st, st.Path, out, time.Hour, 2, log); done <- struct{}{} }()
	<-done
	<-done
	if n := len(autoBackups(st.Path, out)); n != 1 {
		t.Errorf("%d backups, want 1", n)
	}
	if c, err := st.CommitCosts(context.Background(), store.Filter{}, 0); err != nil || len(c) != 1 {
		t.Errorf("%+v %v", c, err)
	}
}

func TestRefresherRecordsAFailure(t *testing.T) {
	p, err := source.NewFallbackPricer()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the download fails at once and the loop returns
	status := &web.PriceStatus{}
	runModelsRefresher(ctx, filepath.Join(t.TempDir(), "models.json"), time.Hour, p, tempStore(t), status,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

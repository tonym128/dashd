package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/scan"
	"github.com/tonym128/dashd/internal/source"
	"github.com/tonym128/dashd/internal/store"
	"github.com/tonym128/dashd/internal/web"
)

func TestRemoteSinkPushesToIngestEndpoint(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv, err := web.New(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(web.WithAuth("push-secret", nil, srv.Handler()))
	defer ts.Close()

	sink := newRemoteSink(ts.URL, "push-secret", "devbox")
	sess := model.SessionWrite{
		Session: model.Session{
			UID:     "push-sess-1",
			Agent:   model.AgentCodex,
			Project: "/src/dashd",
		},
		Path: "/home/tony/.codex/sessions/push-sess-1.jsonl",
		Calls: []model.Call{{
			SessionUID:   "push-sess-1",
			Agent:        model.AgentCodex,
			Project:      "/src/dashd",
			Model:        "gpt-5.4",
			CallKey:      "k1",
			Time:         time.Date(2026, 11, 17, 15, 0, 0, 0, time.UTC),
			InputTokens:  200,
			OutputTokens: 100,
			TotalTokens:  300,
			CostUSD:      0.15,
			Priced:       true,
		}},
	}

	if err := sink.ReplaceSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := sink.RecomputeSession(sess.UID); err != nil {
		t.Fatal(err)
	}

	machines, err := st.Machines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 1 || machines[0] != "devbox" {
		t.Fatalf("remote Machines() = %v, want [devbox]", machines)
	}

	// Wrong token must fail so the caller does not advance cursors.
	badSink := newRemoteSink(ts.URL, "wrong-token", "devbox")
	_ = badSink.ReplaceSession(sess)
	if err := badSink.RecomputeSession(sess.UID); err == nil {
		t.Fatal("expected error with wrong auth token, got nil")
	}

	// SyncModels + AppendSession + SendHeartbeat + runDoctor.
	dir := t.TempDir()
	srvModels := filepath.Join(dir, "srv-models.json")
	if err := os.WriteFile(srvModels, []byte(`{"data":[{"id":"google/gemini-4-hydrogen","pricing":{"prompt":"0.0000015","completion":"0.0000075"}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.ModelsPath = srvModels

	cliModels := filepath.Join(dir, "cli-models.json")
	pricer, err := source.NewFallbackPricer()
	if err != nil {
		t.Fatal(err)
	}
	syncSink := newRemoteSink(ts.URL, "push-secret", "devbox").withModelsSync(cliModels, pricer, nil)
	syncSink.SyncModels()
	if _, ok := pricer.Resolve("google/gemini-4-hydrogen"); !ok {
		t.Fatal("expected SyncModels to reload pricer with server model")
	}

	if err := syncSink.AppendSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := syncSink.RecomputeSession(sess.UID); err != nil {
		t.Fatal(err)
	}
	if err := syncSink.SendHeartbeat(); err != nil {
		t.Fatal(err)
	}

	var docOut bytes.Buffer
	if err := runDoctor(ctx, options{
		db:        filepath.Join(dir, "doc.db"),
		home:      dir,
		models:    cliModels,
		machine:   "devbox",
		remote:    ts.URL,
		authToken: "push-secret",
	}, &docOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(docOut.String(), "dashd doctor") || !strings.Contains(docOut.String(), "200") {
		t.Fatalf("unexpected doctor output: %s", docOut.String())
	}

	// runPushLoop exits cleanly when context is canceled.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	sc := scan.New(scan.Config{Store: st, Pricer: pricer})
	runPushLoop(cctx, sc, syncSink, time.Second, nil)
}

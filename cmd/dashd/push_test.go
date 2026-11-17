package main

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
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
}

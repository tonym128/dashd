package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/store"
	"github.com/tonym128/dashd/internal/web"
)

func TestLocalProxySinkAndRecompute(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	sink := &localProxySink{st: st}
	now := time.Now().UTC().Truncate(time.Second)

	for i := 0; i < 3; i++ {
		err := sink.RecordCall(model.Call{
			SessionUID:   "ollama:cachiest:2026-11-28",
			CallKey:      fmt.Sprintf("k%d", i),
			Agent:        "ollama",
			Project:      "/proj/local",
			Machine:      "cachiest",
			Model:        "qwen3-coder-30b-a3b",
			Time:         now.Add(time.Duration(i) * time.Second),
			Day:          "2026-11-28",
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
			LLMSeconds:   1.5,
			CostUSD:      0,
			Priced:       true,
		}, "ollama (2026-11-28)")
		if err != nil {
			t.Fatalf("RecordCall %d: %v", i, err)
		}
	}

	tot, err := st.Totals(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if tot.Sessions != 1 || tot.Calls != 3 || tot.TotalTokens != 450 || tot.UnpricedCalls != 0 {
		t.Fatalf("Totals = %+v, want sessions=1 calls=3 tokens=450 unpriced=0", tot)
	}
}

func TestRemoteProxySinkEndToEndAndRequeue(t *testing.T) {
	// Real dashd server receiving /api/ingest
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := web.New(st, log)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	srv.IngestEnabled = true
	h := web.WithAuthConfig(web.AuthConfig{
		UIToken:     "ui-tok",
		IngestToken: "ingest-tok",
		Log:         log,
	}, srv.Handler())
	ts := httptest.NewServer(h)
	defer ts.Close()

	// Wrong token fails and requeues the batch.
	badSink := newRemoteProxySink(ts.URL, "wrong-tok", "cachiest", log)
	now := time.Now().UTC().Truncate(time.Second)
	call := model.Call{
		SessionUID:   "llama-server:cachiest:2026-11-28",
		CallKey:      "c1",
		Agent:        "llama-server",
		Machine:      "cachiest",
		Model:        "qwen3-coder-30b-a3b",
		Time:         now,
		Day:          now.Format("2006-01-02"),
		InputTokens:  200,
		OutputTokens: 100,
		TotalTokens:  300,
		LLMSeconds:   2.0,
		Priced:       true,
	}
	if err := badSink.RecordCall(call, "llama-server"); err != nil {
		t.Fatalf("RecordCall: %v", err)
	}
	if err := badSink.Flush(context.Background()); err == nil {
		t.Fatal("Flush with wrong token succeeded, want error")
	}
	// Fix token and flush again — requeued item must succeed!
	badSink.token = "ingest-tok"
	if err := badSink.Flush(context.Background()); err != nil {
		t.Fatalf("Flush after requeue failed: %v", err)
	}

	tot, err := st.Totals(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if tot.Calls != 1 || tot.TotalTokens != 300 {
		t.Fatalf("remote store Totals = %+v, want calls=1 tokens=300", tot)
	}
}

func TestRunProxyValidationAndEndToEnd(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var out bytes.Buffer

	// Missing upstream
	if err := runProxy(context.Background(), options{listen: "127.0.0.1:0"}, nil, log, &out); err == nil {
		t.Fatal("runProxy without -upstream succeeded, want error")
	}
	// Bad upstream
	if err := runProxy(context.Background(), options{upstream: "not-a-url", listen: "127.0.0.1:0"}, nil, log, &out); err == nil {
		t.Fatal("runProxy with invalid -upstream succeeded, want error")
	}

	// Mock upstream Ollama server
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"llama3.3","done":true,"prompt_eval_count":50,"eval_count":25,"eval_duration":500000000}`)
	}))
	defer upstream.Close()

	// Real local store
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var buf syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- runProxy(ctx, options{
			upstream:   upstream.URL,
			listen:     "127.0.0.1:0",
			agent:      "ollama",
			machine:    "testbox",
			skipAgents: "opencode",
			free:       true,
		}, st, log, &buf)
	}()

	// Wait for proxy to print its listening address.
	var listenURL string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := buf.String()
		if idx := strings.Index(s, "http://127.0.0.1:"); idx >= 0 {
			rest := s[idx:]
			if sp := strings.IndexByte(rest, ' '); sp > 0 {
				listenURL = rest[:sp]
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if listenURL == "" {
		t.Fatalf("proxy never printed listen URL; output=%q", buf.String())
	}

	// Send one recorded request and one skipped (opencode) request.
	resp, err := http.Post(listenURL+"/api/generate", "application/json", strings.NewReader(`{"model":"llama3.3"}`))
	if err != nil {
		t.Fatalf("POST through proxy: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	reqSkip, _ := http.NewRequest(http.MethodPost, listenURL+"/api/generate", strings.NewReader(`{"model":"llama3.3"}`))
	reqSkip.Header.Set("User-Agent", "opencode/1.0")
	respSkip, err := http.DefaultClient.Do(reqSkip)
	if err != nil {
		t.Fatalf("POST skipped through proxy: %v", err)
	}
	io.Copy(io.Discard, respSkip.Body)
	respSkip.Body.Close()

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runProxy exit error: %v", err)
	}

	tot, err := st.Totals(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if tot.Calls != 1 || tot.InputTokens != 50 || tot.OutputTokens != 25 || tot.UnpricedCalls != 0 {
		t.Fatalf("Totals after proxy = %+v, want calls=1 in=50 out=25 unpriced=0", tot)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

package proxy

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
)

type memSink struct {
	mu     sync.Mutex
	calls  []model.Call
	titles []string
	err    error
}

func (m *memSink) RecordCall(call model.Call, title string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.calls = append(m.calls, call)
	m.titles = append(m.titles, title)
	return nil
}

func (m *memSink) snapshot() ([]model.Call, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := append([]model.Call(nil), m.calls...)
	t := append([]string(nil), m.titles...)
	return c, t
}

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

func TestInferAgent(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"http://127.0.0.1:11434", "ollama"},
		{"http://ollama.local:8000", "ollama"},
		{"http://127.0.0.1:8080", "llama-server"},
		{"http://llama-swap:9000", "llama-server"},
		{"https://api.anthropic.com", "anthropic-proxy"},
		{"https://api.openai.com", "openai-proxy"},
		{"http://127.0.0.1:9999", "proxy"},
	}
	for _, tc := range cases {
		if got := InferAgent(mustURL(t, tc.url)); got != tc.want {
			t.Errorf("InferAgent(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
	if got := InferAgent(nil); got != "proxy" {
		t.Errorf("InferAgent(nil) = %q, want proxy", got)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with nil Upstream succeeded, want error")
	}
	if _, err := New(Config{Upstream: &url.URL{Path: "/only-path"}}); err == nil {
		t.Fatal("New with hostless Upstream succeeded, want error")
	}
}

func TestOpenAINonStreamingAndLlamaTimings(t *testing.T) {
	var seenHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			// OpenAI usage with cached + reasoning + llama-server timings.
			io.WriteString(w, `{
				"id":"chatcmpl-1",
				"model":"qwen3-coder-30b-a3b",
				"choices":[{"message":{"role":"assistant","content":"hi"}}],
				"usage":{
					"prompt_tokens":1000,
					"completion_tokens":300,
					"total_tokens":1300,
					"prompt_tokens_details":{"cached_tokens":400},
					"completion_tokens_details":{"reasoning_tokens":100}
				},
				"timings":{"prompt_n":600,"cache_n":400,"predicted_n":300,"prompt_ms":500,"predicted_ms":2500}
			}`)
		case "/v1/completions":
			// llama-server timings ONLY (no usage block).
			io.WriteString(w, `{
				"content":"ok",
				"timings":{"prompt_n":250,"cache_n":50,"predicted_n":120,"prompt_ms":200,"predicted_ms":1000}
			}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	sink := &memSink{}
	fixed := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	p, err := New(Config{
		Upstream:       mustURL(t, upstream.URL),
		Agent:          "llama-server",
		Machine:        "cachiest",
		DefaultProject: "/proj/default",
		Free:           true,
		Sink:           sink,
		Now:            func() time.Time { return fixed },
		Log:            testLog(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	// 1. /v1/chat/completions with attribution headers
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"qwen3-coder-30b-a3b","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dashd-Project", "/proj/custom")
	req.Header.Set("X-Dashd-Session", "my-sess")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST chat/completions: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Verify internal headers were stripped before reaching upstream.
	for _, h := range []string{"X-Dashd-Project", "X-Dashd-Session", "X-Dashd-Skip", "X-Dashd-Agent"} {
		if seenHeaders.Get(h) != "" {
			t.Errorf("upstream received stripped header %s=%q", h, seenHeaders.Get(h))
		}
	}

	// 2. /v1/completions with timings only (model falls back to request body)
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/completions", strings.NewReader(`{"model":"local-gguf","prompt":"hello"}`))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("POST completions: %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	srv.Close()

	calls, _ := sink.snapshot()
	if len(calls) != 2 {
		t.Fatalf("recorded %d calls, want 2", len(calls))
	}
	c0 := calls[0]
	if c0.Model != "qwen3-coder-30b-a3b" || c0.InputTokens != 600 || c0.CacheReadTokens != 400 ||
		c0.OutputTokens != 200 || c0.ReasoningTokens != 100 || c0.TotalTokens != 1300 {
		t.Errorf("call[0] tokens mismatch: %+v", c0)
	}
	if c0.LLMSeconds != 3.0 {
		t.Errorf("call[0] LLMSeconds = %v, want 3.0", c0.LLMSeconds)
	}
	if c0.Project != "/proj/custom" || c0.SessionUID != "llama-server:my-sess" || !c0.Priced || c0.CostUSD != 0 {
		t.Errorf("call[0] metadata mismatch: %+v", c0)
	}

	c1 := calls[1]
	if c1.Model != "local-gguf" || c1.InputTokens != 250 || c1.CacheReadTokens != 50 || c1.OutputTokens != 120 || c1.LLMSeconds != 1.2 {
		t.Errorf("call[1] timings fallback mismatch: %+v", c1)
	}
	if c1.Project != "/proj/default" {
		t.Errorf("call[1] project = %q, want /proj/default", c1.Project)
	}
}

func TestOpenAIStreamingSSEAndResponsesAPI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		switch r.URL.Path {
		case "/v1/chat/completions":
			chunks := []string{
				`data: {"id":"1","model":"gpt-4o","choices":[{"delta":{"content":"Hel"}}]}`,
				`data: {"id":"1","model":"gpt-4o","choices":[{"delta":{"content":"lo"}}]}`,
				`data: {"id":"1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":500,"completion_tokens":80,"total_tokens":580}}`,
				`data: [DONE]`,
			}
			for _, line := range chunks {
				fmt.Fprintln(w, line)
				fmt.Fprintln(w)
				if flusher != nil {
					flusher.Flush()
				}
			}
		case "/v1/responses":
			fmt.Fprintln(w, `event: response.completed`)
			fmt.Fprintln(w, `data: {"type":"response.completed","response":{"model":"o4-mini","usage":{"input_tokens":800,"output_tokens":200,"input_tokens_details":{"cached_tokens":200},"output_tokens_details":{"reasoning_tokens":50}}}}`)
			fmt.Fprintln(w)
		}
	}))
	defer upstream.Close()

	sink := &memSink{}
	p, err := New(Config{
		Upstream: mustURL(t, upstream.URL),
		Agent:    "openai-proxy",
		Machine:  "dev",
		Pricer: func(m string, in, out, cr, cw, cw1h int, at time.Time) (float64, bool) {
			return 0.0123, true
		},
		Sink: sink,
		Log:  testLog(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	// Chat completions SSE
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-4o","stream":true}`))
	if err != nil {
		t.Fatalf("POST SSE: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `data: [DONE]`) {
		t.Fatalf("client did not receive full SSE stream: %q", string(body))
	}

	// Responses API SSE
	resp2, err := http.Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"o4-mini","stream":true}`))
	if err != nil {
		t.Fatalf("POST responses SSE: %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	srv.Close()

	calls, _ := sink.snapshot()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Model != "gpt-4o" || calls[0].InputTokens != 500 || calls[0].OutputTokens != 80 || calls[0].CostUSD != 0.0123 || !calls[0].Priced {
		t.Errorf("SSE chat call mismatch: %+v", calls[0])
	}
	if calls[1].Model != "o4-mini" || calls[1].InputTokens != 600 || calls[1].CacheReadTokens != 200 ||
		calls[1].OutputTokens != 150 || calls[1].ReasoningTokens != 50 {
		t.Errorf("SSE responses call mismatch: %+v", calls[1])
	}
}

func TestOllamaStreamingNDJSONAndNonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			w.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprintln(w, `{"model":"llama3.3:70b","message":{"role":"assistant","content":"Hello"},"done":false}`)
			fmt.Fprintln(w, `{"model":"llama3.3:70b","message":{"role":"assistant","content":" world"},"done":true,"prompt_eval_count":120,"eval_count":45,"prompt_eval_duration":200000000,"eval_duration":1800000000}`)
		case "/api/generate":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"model":"qwen3:8b","response":"42","done":true,"prompt_eval_count":30,"eval_count":10,"total_duration":500000000}`)
		}
	}))
	defer upstream.Close()

	sink := &memSink{}
	p, err := New(Config{
		Upstream: mustURL(t, upstream.URL),
		Agent:    "ollama",
		Machine:  "cachiest",
		Free:     true,
		Sink:     sink,
		Log:      testLog(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	r1, err := http.Post(srv.URL+"/api/chat", "application/json", strings.NewReader(`{"model":"llama3.3:70b"}`))
	if err != nil {
		t.Fatalf("POST /api/chat: %v", err)
	}
	io.Copy(io.Discard, r1.Body)
	r1.Body.Close()

	r2, err := http.Post(srv.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"qwen3:8b"}`))
	if err != nil {
		t.Fatalf("POST /api/generate: %v", err)
	}
	io.Copy(io.Discard, r2.Body)
	r2.Body.Close()
	srv.Close()

	calls, _ := sink.snapshot()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Model != "llama3.3:70b" || calls[0].InputTokens != 120 || calls[0].OutputTokens != 45 || calls[0].LLMSeconds != 2.0 {
		t.Errorf("ollama /api/chat mismatch: %+v", calls[0])
	}
	if calls[1].Model != "qwen3:8b" || calls[1].InputTokens != 30 || calls[1].OutputTokens != 10 || calls[1].LLMSeconds != 0.5 {
		t.Errorf("ollama /api/generate mismatch: %+v", calls[1])
	}
}

func TestAnthropicJSONAndStreamingSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Mode") == "stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `event: message_start`)
			fmt.Fprintln(w, `data: {"type":"message_start","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":400,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200,"output_tokens":1}}}`)
			fmt.Fprintln(w)
			fmt.Fprintln(w, `event: content_block_delta`)
			fmt.Fprintln(w, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}`)
			fmt.Fprintln(w)
			fmt.Fprintln(w, `event: message_delta`)
			fmt.Fprintln(w, `data: {"type":"message_delta","usage":{"output_tokens":150}}`)
			fmt.Fprintln(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"model":"claude-opus-4-6","usage":{"input_tokens":300,"output_tokens":90,"cache_read_input_tokens":500,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_1h_input_tokens":100}}}`)
	}))
	defer upstream.Close()

	sink := &memSink{}
	p, err := New(Config{
		Upstream: mustURL(t, upstream.URL),
		Agent:    "anthropic-proxy",
		Sink:     sink,
		Log:      testLog(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	// Non-streaming
	r1, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{}`))
	resp1, err := http.DefaultClient.Do(r1)
	if err != nil {
		t.Fatalf("anthropic JSON: %v", err)
	}
	io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()

	// Streaming
	r2, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	r2.Header.Set("X-Mode", "stream")
	resp2, err := http.DefaultClient.Do(r2)
	if err != nil {
		t.Fatalf("anthropic SSE: %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	srv.Close()

	calls, _ := sink.snapshot()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Model != "claude-opus-4-6" || calls[0].InputTokens != 300 || calls[0].OutputTokens != 90 ||
		calls[0].CacheReadTokens != 500 || calls[0].CacheWriteTokens != 100 || calls[0].CacheWrite1hTokens != 100 {
		t.Errorf("anthropic JSON call mismatch: %+v", calls[0])
	}
	if calls[1].Model != "claude-sonnet-4-6" || calls[1].InputTokens != 400 || calls[1].OutputTokens != 150 ||
		calls[1].CacheReadTokens != 1000 || calls[1].CacheWriteTokens != 200 {
		t.Errorf("anthropic SSE call mismatch: %+v", calls[1])
	}
}

func TestDeduplicationBypassAndNonInferenceRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[]}`)
			return
		}
		if r.Header.Get("X-Fail") == "1" {
			http.Error(w, "upstream overloaded", http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{"model":"qwen3","usage":{"prompt_tokens":100,"completion_tokens":50}}`)
	}))
	defer upstream.Close()

	sink := &memSink{}
	p, err := New(Config{
		Upstream:   mustURL(t, upstream.URL),
		Agent:      "ollama",
		SkipAgents: DefaultSkipAgents,
		Sink:       sink,
		Log:        testLog(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	send := func(method, path string, headers map[string]string) int {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(`{"model":"qwen3"}`))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	// Non-inference GET /v1/models -> proxied, not recorded.
	if code := send(http.MethodGet, "/v1/models", nil); code != 200 {
		t.Fatalf("GET /v1/models = %d", code)
	}
	// X-Dashd-Skip: 1 -> proxied, skipped.
	if code := send(http.MethodPost, "/v1/chat/completions", map[string]string{"X-Dashd-Skip": "1"}); code != 200 {
		t.Fatalf("X-Dashd-Skip = %d", code)
	}
	// Known harness User-Agent (opencode, claude-code) -> proxied, skipped.
	if code := send(http.MethodPost, "/v1/chat/completions", map[string]string{"User-Agent": "opencode/1.2.3"}); code != 200 {
		t.Fatalf("opencode UA = %d", code)
	}
	if code := send(http.MethodPost, "/v1/chat/completions", map[string]string{"User-Agent": "Claude-Code/2.0"}); code != 200 {
		t.Fatalf("claude-code UA = %d", code)
	}
	// Upstream 429 error -> proxied, not recorded.
	if code := send(http.MethodPost, "/v1/chat/completions", map[string]string{"X-Fail": "1"}); code != 429 {
		t.Fatalf("429 = %d", code)
	}
	// Normal uninstrumented client (curl / Open WebUI) -> recorded!
	if code := send(http.MethodPost, "/v1/chat/completions", map[string]string{"User-Agent": "curl/8.10", "X-Dashd-Agent": "custom-script"}); code != 200 {
		t.Fatalf("curl = %d", code)
	}
	srv.Close()

	calls, _ := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls, want exactly 1: %+v", len(calls), calls)
	}
	if calls[0].Agent != "custom-script" {
		t.Errorf("agent = %q, want custom-script", calls[0].Agent)
	}
	st := p.Stats()
	if st.Recorded != 1 || st.Skipped != 3 || st.Proxied != 6 {
		t.Errorf("stats = %+v, want Recorded=1 Skipped=3 Proxied=6", st)
	}
}

func TestGzipResponseAndLargeStreamCaptureAndErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			if r.Header.Get("X-Case") == "gzip" {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", "gzip")
				gz := gzip.NewWriter(w)
				io.WriteString(gz, `{"model":"gz-model","usage":{"prompt_tokens":42,"completion_tokens":18}}`)
				gz.Close()
				return
			}
			// Large streaming response (> maxCaptureBytes) to verify head+tail capture preserves final usage.
			w.Header().Set("Content-Type", "text/event-stream")
			pad := strings.Repeat("x", 64*1024)
			for i := 0; i < 40; i++ {
				fmt.Fprintf(w, "data: {\"model\":\"big-model\",\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", pad)
			}
			fmt.Fprintln(w, `data: {"model":"big-model","choices":[],"usage":{"prompt_tokens":999,"completion_tokens":777}}`)
			fmt.Fprintln(w, `data: [DONE]`)
		}
	}))
	defer upstream.Close()

	sink := &memSink{}
	p, err := New(Config{Upstream: mustURL(t, upstream.URL), Agent: "proxy", Sink: sink, Log: testLog()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	// Gzipped JSON response (disable automatic client decompression so proxy sees Content-Encoding: gzip).
	tr := &http.Transport{DisableCompression: true}
	cli := &http.Client{Transport: tr}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("X-Case", "gzip")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("gzip request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Large stream > 2.5 MiB
	resp2, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("large stream: %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	// Sink error increments Error counter without breaking client response.
	sink.mu.Lock()
	sink.err = errors.New("db locked")
	sink.mu.Unlock()
	req3, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	req3.Header.Set("X-Case", "gzip")
	req3.Header.Set("Accept-Encoding", "gzip")
	resp3, err := cli.Do(req3)
	if err != nil {
		t.Fatalf("sink-error request: %v", err)
	}
	io.Copy(io.Discard, resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Errorf("client status on sink error = %d, want 200", resp3.StatusCode)
	}
	srv.Close()

	calls, _ := sink.snapshot()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Model != "gz-model" || calls[0].InputTokens != 42 || calls[0].OutputTokens != 18 {
		t.Errorf("gzip call mismatch: %+v", calls[0])
	}
	if calls[1].Model != "big-model" || calls[1].InputTokens != 999 || calls[1].OutputTokens != 777 {
		t.Errorf("large stream call mismatch: %+v", calls[1])
	}
	if p.Stats().Errors != 1 {
		t.Errorf("stats.Errors = %d, want 1", p.Stats().Errors)
	}
}

func TestConcurrentProxyRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"concurrent-m","usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer upstream.Close()

	sink := &memSink{}
	p, err := New(Config{Upstream: mustURL(t, upstream.URL), Agent: "ollama", Free: true, Sink: sink, Log: testLog()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewBufferString(`{"model":"concurrent-m"}`))
			if err != nil {
				t.Errorf("concurrent POST: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	srv.Close()

	calls, _ := sink.snapshot()
	if len(calls) != n {
		t.Fatalf("got %d calls, want %d", len(calls), n)
	}
	// Every CallKey must be unique so SQLite never collapses concurrent calls.
	keys := map[string]bool{}
	for _, c := range calls {
		if keys[c.CallKey] {
			t.Fatalf("duplicate CallKey %q", c.CallKey)
		}
		keys[c.CallKey] = true
	}
}

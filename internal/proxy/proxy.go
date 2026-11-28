// Package proxy implements a transparent HTTP pass-through proxy for local and
// cloud LLM servers (OpenAI-compatible, llama-server, Ollama, Anthropic).
//
// Requests and streaming responses (SSE and NDJSON) are forwarded byte-for-byte
// with immediate flushing so latency is unaffected. Completed calls are parsed
// for token usage and timing, then emitted as model.Call records to a CallSink.
package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tonym128/dashd/internal/model"
)

// DefaultSkipAgents lists User-Agent substrings of coding-agent harnesses whose
// session logs dashd already scans. Skipping them prevents double-counting when
// a harness shares the same proxy as uninstrumented clients.
var DefaultSkipAgents = []string{
	"opencode",
	"claude-code",
	"claude-cli",
	"codex",
	"gemini-cli",
	"antigravity",
	"pi-agent",
}

// CallSink receives each completed LLM call captured by the proxy.
type CallSink interface {
	RecordCall(call model.Call, sessionTitle string) error
}

// PriceFunc prices a call from its token counts.
type PriceFunc func(model string, in, out, cacheRead, cacheWrite, cacheWrite1h int, at time.Time) (float64, bool)

// Config configures a Proxy.
type Config struct {
	// Upstream is the target LLM server URL (e.g. http://127.0.0.1:11434).
	Upstream *url.URL
	// Agent is the default agent label attached to recorded calls (e.g. "ollama",
	// "llama-server", "proxy"). Overridable per request with X-Dashd-Agent.
	Agent string
	// Machine is the machine name attached to recorded calls.
	Machine string
	// DefaultProject is the project label when X-Dashd-Project is not set.
	DefaultProject string
	// SkipAgents is a list of case-insensitive User-Agent substrings to bypass
	// recording (prevents duplicating harnesses that dashd already scans).
	SkipAgents []string
	// Free marks every captured call as $0.00 (Priced=true), suitable for local
	// engines like ollama and llama-server so they are not flagged as unpriced.
	Free bool
	// Pricer prices non-free calls. Optional.
	Pricer PriceFunc
	// Sink receives recorded calls.
	Sink CallSink
	// Transport overrides the upstream HTTP round-tripper (useful for tests).
	Transport http.RoundTripper
	// Now is injectable for tests.
	Now func() time.Time
	Log *slog.Logger
}

// Stats exposes runtime counters for diagnostics and tests.
type Stats struct {
	Proxied  int64
	Recorded int64
	Skipped  int64
	Errors   int64
}

// Proxy is an http.Handler that transparently forwards requests to Upstream
// while capturing token usage and timing for LLM inference endpoints.
type Proxy struct {
	cfg     Config
	log     *slog.Logger
	reverse *httputil.ReverseProxy
	skipUA  []string
	now     func() time.Time

	seq      atomic.Int64
	proxied  atomic.Int64
	recorded atomic.Int64
	skipped  atomic.Int64
	errors   atomic.Int64
}

// New constructs a Proxy for cfg.
func New(cfg Config) (*Proxy, error) {
	if cfg.Upstream == nil || cfg.Upstream.Scheme == "" || cfg.Upstream.Host == "" {
		return nil, fmt.Errorf("proxy upstream URL must include scheme and host")
	}
	if cfg.Agent == "" {
		cfg.Agent = InferAgent(cfg.Upstream)
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	skipUA := make([]string, 0, len(cfg.SkipAgents))
	for _, s := range cfg.SkipAgents {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			skipUA = append(skipUA, s)
		}
	}

	p := &Proxy{
		cfg:    cfg,
		log:    cfg.Log,
		skipUA: skipUA,
		now:    cfg.Now,
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(cfg.Upstream)
			// Strip dashd-internal attribution headers before forwarding upstream.
			pr.Out.Header.Del("X-Dashd-Skip")
			pr.Out.Header.Del("X-Dashd-Project")
			pr.Out.Header.Del("X-Dashd-Session")
			pr.Out.Header.Del("X-Dashd-Agent")
		},
		// Flush immediately for SSE/NDJSON streaming responses.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.errors.Add(1)
			p.log.Warn("proxy upstream error", "path", r.URL.Path, "error", err)
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}
	if cfg.Transport != nil {
		rp.Transport = cfg.Transport
	}
	p.reverse = rp
	return p, nil
}

// InferAgent picks a sensible default agent name from the upstream URL.
func InferAgent(u *url.URL) string {
	if u == nil {
		return "proxy"
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	switch {
	case port == "11434" || strings.Contains(host, "ollama"):
		return "ollama"
	case port == "8080" || strings.Contains(host, "llama"):
		return "llama-server"
	case strings.Contains(host, "anthropic"):
		return "anthropic-proxy"
	case strings.Contains(host, "openai"):
		return "openai-proxy"
	default:
		return "proxy"
	}
}

// Stats returns a snapshot of the proxy's counters.
func (p *Proxy) Stats() Stats {
	return Stats{
		Proxied:  p.proxied.Load(),
		Recorded: p.recorded.Load(),
		Skipped:  p.skipped.Load(),
		Errors:   p.errors.Load(),
	}
}

// ServeHTTP forwards r to Upstream and records LLM call usage when applicable.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.proxied.Add(1)

	kind := detectEndpoint(r.Method, r.URL.Path)
	if kind == endpointNone || p.shouldSkip(r) {
		if kind != endpointNone {
			p.skipped.Add(1)
		}
		p.reverse.ServeHTTP(w, r)
		return
	}

	// Capture attribution headers before Director strips them.
	reqMeta := requestMeta{
		agent:   firstNonEmpty(strings.TrimSpace(r.Header.Get("X-Dashd-Agent")), p.cfg.Agent),
		project: firstNonEmpty(strings.TrimSpace(r.Header.Get("X-Dashd-Project")), p.cfg.DefaultProject),
		session: strings.TrimSpace(r.Header.Get("X-Dashd-Session")),
	}
	// Peek request body for fallback model name (some endpoints omit model in
	// streaming deltas or error-free short responses).
	if r.Body != nil {
		reqMeta.reqModel = peekRequestModel(r)
	}

	start := p.now()
	capW := newCaptureResponseWriter(w)
	p.reverse.ServeHTTP(capW, r)
	elapsed := p.now().Sub(start)

	if capW.status < 200 || capW.status >= 300 {
		return
	}

	raw := capW.bytes()
	if strings.EqualFold(capW.Header().Get("Content-Encoding"), "gzip") {
		if decompressed, err := gunzip(raw); err == nil {
			raw = decompressed
		}
	}

	usage, ok := parseResponse(kind, capW.Header().Get("Content-Type"), raw)
	if !ok {
		return
	}
	if usage.Model == "" {
		usage.Model = reqMeta.reqModel
	}
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.ReasoningTokens == 0 &&
		usage.CacheReadTokens == 0 && usage.CacheWriteTokens == 0 {
		return
	}

	now := p.now()
	llmSecs := usage.LLMSeconds
	if llmSecs <= 0 {
		llmSecs = elapsed.Seconds()
	}

	sessionUID := reqMeta.session
	sessionTitle := ""
	if sessionUID == "" {
		day := now.Local().Format("2006-01-02")
		machine := p.cfg.Machine
		if machine == "" {
			machine = "local"
		}
		sessionUID = fmt.Sprintf("%s:%s:%s", reqMeta.agent, machine, day)
		sessionTitle = fmt.Sprintf("%s (%s)", reqMeta.agent, day)
	} else {
		sessionTitle = sessionUID
		if !strings.Contains(sessionUID, ":") {
			sessionUID = reqMeta.agent + ":" + sessionUID
		}
	}

	seq := p.seq.Add(1)
	callKey := fmt.Sprintf("p%d-%d", now.UnixNano(), seq)
	total := usage.InputTokens + usage.OutputTokens + usage.CacheReadTokens + usage.CacheWriteTokens + usage.ReasoningTokens

	call := model.Call{
		SessionUID:         sessionUID,
		CallKey:            callKey,
		Agent:              reqMeta.agent,
		Project:            reqMeta.project,
		Machine:            p.cfg.Machine,
		Model:              usage.Model,
		Time:               now,
		Day:                now.Local().Format("2006-01-02"),
		InputTokens:        usage.InputTokens,
		OutputTokens:       usage.OutputTokens,
		CacheReadTokens:    usage.CacheReadTokens,
		CacheWriteTokens:   usage.CacheWriteTokens,
		CacheWrite1hTokens: usage.CacheWrite1hTokens,
		ReasoningTokens:    usage.ReasoningTokens,
		TotalTokens:        total,
		LLMSeconds:         llmSecs,
	}

	if p.cfg.Free {
		call.CostUSD = 0
		call.Priced = true
	} else if p.cfg.Pricer != nil {
		generated := call.OutputTokens + call.ReasoningTokens
		call.CostUSD, call.Priced = p.cfg.Pricer(call.Model, call.InputTokens, generated,
			call.CacheReadTokens, call.CacheWriteTokens, call.CacheWrite1hTokens, call.Time)
	}

	if p.cfg.Sink != nil {
		if err := p.cfg.Sink.RecordCall(call, sessionTitle); err != nil {
			p.errors.Add(1)
			p.log.Warn("proxy failed to record call", "model", call.Model, "error", err)
			return
		}
	}
	p.recorded.Add(1)
}

func (p *Proxy) shouldSkip(r *http.Request) bool {
	if skip := strings.TrimSpace(r.Header.Get("X-Dashd-Skip")); skip != "" && skip != "0" && !strings.EqualFold(skip, "false") {
		p.log.Debug("proxy skipping call (X-Dashd-Skip)", "path", r.URL.Path)
		return true
	}
	if len(p.skipUA) == 0 {
		return false
	}
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	if ua == "" {
		return false
	}
	for _, sub := range p.skipUA {
		if strings.Contains(ua, sub) {
			p.log.Debug("proxy skipping call (harness User-Agent)", "ua", ua, "match", sub)
			return true
		}
	}
	return false
}

type requestMeta struct {
	agent    string
	project  string
	session  string
	reqModel string
}

// maxCaptureBytes bounds how much of a response body is retained in memory for
// usage extraction. Even very long streaming responses keep their usage chunk
// at the tail; if a response exceeds this cap we keep both the head (for
// Anthropic message_start) and the tail (for final usage/timings).
const maxCaptureBytes = 2 << 20 // 2 MiB

type captureResponseWriter struct {
	http.ResponseWriter
	status int
	mu     sync.Mutex
	head   bytes.Buffer
	tail   bytes.Buffer
	total  int
}

func newCaptureResponseWriter(w http.ResponseWriter) *captureResponseWriter {
	return &captureResponseWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *captureResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureResponseWriter) Write(p []byte) (int, error) {
	w.capture(p)
	return w.ResponseWriter.Write(p)
}

func (w *captureResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *captureResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *captureResponseWriter) capture(p []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total += len(p)
	const headLimit = 64 << 10 // 64 KiB for Anthropic message_start / initial headers
	if w.head.Len() < headLimit {
		rem := headLimit - w.head.Len()
		if len(p) <= rem {
			w.head.Write(p)
			return
		}
		w.head.Write(p[:rem])
		p = p[rem:]
	}
	// Keep up to maxCaptureBytes in tail; if exceeded, drop oldest half so we
	// always preserve the final usage/timings lines without unbounded growth.
	w.tail.Write(p)
	if w.tail.Len() > maxCaptureBytes {
		b := w.tail.Bytes()
		keep := b[len(b)-maxCaptureBytes/2:]
		var next bytes.Buffer
		next.Write(keep)
		w.tail = next
	}
}

func (w *captureResponseWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tail.Len() == 0 {
		return append([]byte(nil), w.head.Bytes()...)
	}
	out := make([]byte, 0, w.head.Len()+1+w.tail.Len())
	out = append(out, w.head.Bytes()...)
	out = append(out, '\n')
	out = append(out, w.tail.Bytes()...)
	return out
}

// peekRequestModel reads up to 64 KiB of a JSON request body to extract the
// "model" field, then restores r.Body so upstream receives every byte intact.
func peekRequestModel(r *http.Request) string {
	const peekLimit = 64 << 10
	buf, err := io.ReadAll(io.LimitReader(r.Body, peekLimit))
	if err != nil || len(buf) == 0 {
		if len(buf) > 0 {
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))
		}
		return ""
	}
	orig := r.Body
	r.Body = struct {
		io.Reader
		io.Closer
	}{
		Reader: io.MultiReader(bytes.NewReader(buf), orig),
		Closer: orig,
	}

	var partial struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(buf, &partial) == nil && partial.Model != "" {
		return partial.Model
	}
	// Fallback for large request bodies (>64 KiB) where closing brace wasn't in
	// the prefix: scan for `"model":"..."` in the prefix.
	return extractJSONStringField(buf, "model")
}

func extractJSONStringField(buf []byte, key string) string {
	needle := `"` + key + `"`
	idx := bytes.Index(buf, []byte(needle))
	if idx < 0 {
		return ""
	}
	rest := bytes.TrimSpace(buf[idx+len(needle):])
	if len(rest) == 0 || rest[0] != ':' {
		return ""
	}
	rest = bytes.TrimSpace(rest[1:])
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	end := 1
	for end < len(rest) {
		if rest[end] == '\\' {
			end += 2
			continue
		}
		if rest[end] == '"' {
			var s string
			if json.Unmarshal(rest[:end+1], &s) == nil {
				return s
			}
			return ""
		}
		end++
	}
	return ""
}

func gunzip(raw []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	return io.ReadAll(io.LimitReader(gr, maxCaptureBytes*2))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---------------------------------------------------------------- protocol parsing

type endpointKind int

const (
	endpointNone endpointKind = iota
	endpointOpenAI
	endpointOllama
	endpointAnthropic
)

func detectEndpoint(method, path string) endpointKind {
	if method != http.MethodPost {
		return endpointNone
	}
	p := strings.TrimSuffix(path, "/")
	switch {
	case p == "/api/chat" || p == "/api/generate":
		return endpointOllama
	case p == "/v1/messages" || strings.HasSuffix(p, "/v1/messages"):
		return endpointAnthropic
	case p == "/v1/chat/completions" || p == "/v1/completions" || p == "/v1/responses" ||
		p == "/chat/completions" || p == "/completions" ||
		strings.HasSuffix(p, "/chat/completions") || strings.HasSuffix(p, "/v1/responses"):
		return endpointOpenAI
	default:
		return endpointNone
	}
}

type parsedUsage struct {
	Model              string
	InputTokens        int
	OutputTokens       int
	CacheReadTokens    int
	CacheWriteTokens   int
	CacheWrite1hTokens int
	ReasoningTokens    int
	LLMSeconds         float64
}

func parseResponse(kind endpointKind, contentType string, raw []byte) (parsedUsage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return parsedUsage{}, false
	}
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream") ||
		bytes.HasPrefix(raw, []byte("data:")) || bytes.HasPrefix(raw, []byte("event:"))

	switch kind {
	case endpointOllama:
		return parseOllamaResponse(raw)
	case endpointAnthropic:
		if isSSE {
			return parseAnthropicSSE(raw)
		}
		return parseAnthropicJSON(raw)
	case endpointOpenAI:
		if isSSE {
			return parseOpenAISSE(raw)
		}
		return parseOpenAIJSON(raw)
	default:
		return parsedUsage{}, false
	}
}

func parseOpenAIJSON(raw []byte) (parsedUsage, bool) {
	var body struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			InputTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
			OutputTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
		Timings *struct {
			PromptN     int     `json:"prompt_n"`
			PredictedN  int     `json:"predicted_n"`
			CacheN      int     `json:"cache_n"`
			PromptMS    float64 `json:"prompt_ms"`
			PredictedMS float64 `json:"predicted_ms"`
		} `json:"timings"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return parsedUsage{}, false
	}
	u := parsedUsage{Model: body.Model}
	if body.Usage != nil {
		in := body.Usage.PromptTokens
		if in == 0 {
			in = body.Usage.InputTokens
		}
		out := body.Usage.CompletionTokens
		if out == 0 {
			out = body.Usage.OutputTokens
		}
		cached := 0
		if body.Usage.PromptTokensDetails != nil {
			cached = body.Usage.PromptTokensDetails.CachedTokens
		} else if body.Usage.InputTokensDetails != nil {
			cached = body.Usage.InputTokensDetails.CachedTokens
		}
		reasoning := 0
		if body.Usage.CompletionTokensDetails != nil {
			reasoning = body.Usage.CompletionTokensDetails.ReasoningTokens
		} else if body.Usage.OutputTokensDetails != nil {
			reasoning = body.Usage.OutputTokensDetails.ReasoningTokens
		}
		if cached > in {
			cached = in
		}
		if reasoning > out {
			reasoning = out
		}
		u.InputTokens = in - cached
		u.CacheReadTokens = cached
		u.OutputTokens = out - reasoning
		u.ReasoningTokens = reasoning
	}
	if body.Timings != nil {
		// llama-server reports prompt_n (uncached prompt tokens), cache_n,
		// predicted_n, and exact prompt_ms / predicted_ms.
		if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadTokens == 0 {
			u.InputTokens = max(0, body.Timings.PromptN)
			u.CacheReadTokens = max(0, body.Timings.CacheN)
			u.OutputTokens = max(0, body.Timings.PredictedN)
		}
		if body.Timings.PromptMS+body.Timings.PredictedMS > 0 {
			u.LLMSeconds = (body.Timings.PromptMS + body.Timings.PredictedMS) / 1000.0
		}
	}
	return u, hasTokens(u)
}

func parseOpenAISSE(raw []byte) (parsedUsage, bool) {
	var best parsedUsage
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		// Handle OpenAI Responses API `response.completed` wrapper as well as
		// standard chat completion chunks.
		var env struct {
			Model    string          `json:"model"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal([]byte(payload), &env) == nil {
			if env.Model != "" && best.Model == "" {
				best.Model = env.Model
			}
			if len(env.Response) > 0 {
				if u, ok := parseOpenAIJSON(env.Response); ok {
					if u.Model == "" {
						u.Model = best.Model
					}
					best = u
					continue
				}
			}
		}
		if u, ok := parseOpenAIJSON([]byte(payload)); ok {
			if u.Model == "" {
				u.Model = best.Model
			}
			best = u
		}
	}
	return best, hasTokens(best)
}

func parseOllamaResponse(raw []byte) (parsedUsage, bool) {
	// Ollama returns either a single JSON object (stream=false) or NDJSON
	// (stream=true) where the final line has "done": true with eval counts.
	var best parsedUsage
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var chunk struct {
			Model              string `json:"model"`
			Done               bool   `json:"done"`
			PromptEvalCount    int    `json:"prompt_eval_count"`
			EvalCount          int    `json:"eval_count"`
			PromptEvalDuration int64  `json:"prompt_eval_duration"` // nanoseconds
			EvalDuration       int64  `json:"eval_duration"`        // nanoseconds
			TotalDuration      int64  `json:"total_duration"`       // nanoseconds
		}
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue
		}
		if chunk.Model != "" {
			best.Model = chunk.Model
		}
		if chunk.Done || chunk.PromptEvalCount > 0 || chunk.EvalCount > 0 {
			if chunk.PromptEvalCount > 0 {
				best.InputTokens = chunk.PromptEvalCount
			}
			if chunk.EvalCount > 0 {
				best.OutputTokens = chunk.EvalCount
			}
			durNS := chunk.PromptEvalDuration + chunk.EvalDuration
			if durNS <= 0 {
				durNS = chunk.TotalDuration
			}
			if durNS > 0 {
				best.LLMSeconds = float64(durNS) / 1e9
			}
		}
	}
	return best, hasTokens(best)
}

func parseAnthropicJSON(raw []byte) (parsedUsage, bool) {
	var body struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheCreation            *struct {
				Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Usage == nil {
		return parsedUsage{}, false
	}
	u := parsedUsage{
		Model:            body.Model,
		InputTokens:      max(0, body.Usage.InputTokens),
		OutputTokens:     max(0, body.Usage.OutputTokens),
		CacheReadTokens:  max(0, body.Usage.CacheReadInputTokens),
		CacheWriteTokens: max(0, body.Usage.CacheCreationInputTokens),
	}
	if body.Usage.CacheCreation != nil {
		u.CacheWrite1hTokens = max(0, body.Usage.CacheCreation.Ephemeral1hInputTokens)
	}
	return u, hasTokens(u)
}

func parseAnthropicSSE(raw []byte) (parsedUsage, bool) {
	var u parsedUsage
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
			Usage   *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if start, ok := parseAnthropicJSON(ev.Message); ok {
				u = start
			} else {
				var m struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(ev.Message, &m) == nil && m.Model != "" {
					u.Model = m.Model
				}
			}
		case "message_delta":
			if ev.Usage != nil {
				if ev.Usage.OutputTokens > 0 {
					u.OutputTokens = ev.Usage.OutputTokens
				}
				if ev.Usage.InputTokens > 0 {
					u.InputTokens = ev.Usage.InputTokens
				}
				if ev.Usage.CacheReadInputTokens > 0 {
					u.CacheReadTokens = ev.Usage.CacheReadInputTokens
				}
				if ev.Usage.CacheCreationInputTokens > 0 {
					u.CacheWriteTokens = ev.Usage.CacheCreationInputTokens
				}
			}
		}
	}
	return u, hasTokens(u)
}

func hasTokens(u parsedUsage) bool {
	return u.InputTokens > 0 || u.OutputTokens > 0 || u.ReasoningTokens > 0 ||
		u.CacheReadTokens > 0 || u.CacheWriteTokens > 0
}

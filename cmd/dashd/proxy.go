package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/proxy"
	"github.com/tonym128/dashd/internal/source"
	"github.com/tonym128/dashd/internal/store"
)

type localProxySink struct {
	mu sync.Mutex
	st *store.Store
}

func (s *localProxySink) RecordCall(call model.Call, sessionTitle string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sw := model.SessionWrite{
		Session: model.Session{
			UID:     call.SessionUID,
			Agent:   call.Agent,
			Project: call.Project,
			Machine: call.Machine,
			Title:   sessionTitle,
		},
		FirstTS: call.Time,
		LastTS:  call.Time,
		Calls:   []model.Call{call},
	}
	if err := s.st.AppendSession(sw); err != nil {
		return err
	}
	return s.st.RecomputeSession(call.SessionUID)
}

type remoteProxySink struct {
	endpoint string
	token    string
	machine  string
	client   *http.Client
	log      *slog.Logger

	mu      sync.Mutex
	pending []model.IngestSession
}

func newRemoteProxySink(remoteURL, token, machine string, log *slog.Logger) *remoteProxySink {
	if log == nil {
		log = slog.Default()
	}
	return &remoteProxySink{
		endpoint: strings.TrimRight(remoteURL, "/") + "/api/ingest",
		token:    token,
		machine:  machine,
		client:   &http.Client{Timeout: 30 * time.Second},
		log:      log,
	}
}

func (s *remoteProxySink) RecordCall(call model.Call, sessionTitle string) error {
	item := model.IngestSession{
		Mode: "append",
		Session: model.SessionWrite{
			Session: model.Session{
				UID:     call.SessionUID,
				Agent:   call.Agent,
				Project: call.Project,
				Machine: call.Machine,
				Title:   sessionTitle,
			},
			FirstTS: call.Time,
			LastTS:  call.Time,
			Calls:   []model.Call{call},
		},
	}
	s.mu.Lock()
	s.pending = append(s.pending, item)
	shouldFlush := len(s.pending) >= 10
	s.mu.Unlock()
	if shouldFlush {
		return s.Flush(context.Background())
	}
	return nil
}

func (s *remoteProxySink) Flush(ctx context.Context) error {
	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return nil
	}
	batch := s.pending
	s.pending = nil
	s.mu.Unlock()

	payload := model.IngestRequest{
		Machine:  s.machine,
		Sessions: batch,
		Version:  version,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		s.requeue(batch)
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.requeue(batch)
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		s.requeue(batch)
		return fmt.Errorf("ingest %s: %d %s", s.endpoint, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func (s *remoteProxySink) requeue(batch []model.IngestSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Bound memory if the central server stays unreachable for a long time.
	const maxQueued = 2000
	combined := append(batch, s.pending...)
	if len(combined) > maxQueued {
		combined = combined[len(combined)-maxQueued:]
	}
	s.pending = combined
}

func runProxy(ctx context.Context, o options, st *store.Store, log *slog.Logger, stdout io.Writer) error {
	if strings.TrimSpace(o.upstream) == "" {
		return fmt.Errorf("-upstream URL is required for proxy (e.g. -upstream http://127.0.0.1:11434)")
	}
	upstreamURL, err := url.Parse(o.upstream)
	if err != nil || upstreamURL.Scheme == "" || upstreamURL.Host == "" {
		return fmt.Errorf("invalid -upstream %q: want http(s)://host:port", o.upstream)
	}

	var sink proxy.CallSink
	var rsink *remoteProxySink
	if o.remote != "" {
		warnInsecureRemote(o.remote, log)
		tok := o.ingestToken
		if tok == "" {
			tok = o.authToken
		}
		rsink = newRemoteProxySink(o.remote, tok, o.machine, log)
		sink = rsink
	} else {
		sink = &localProxySink{st: st}
	}

	var pricerFn proxy.PriceFunc
	if !o.free && o.remote == "" {
		pricer, err := source.NewPricer(o.models, "")
		if err == nil {
			warnIfFallbackOnly(pricer, o.models, log)
			pricerFn = func(m string, in, out, cr, cw, cw1h int, at time.Time) (float64, bool) {
				return pricer.CostCallAt(at, m, in, out, cr, cw, cw1h)
			}
		}
	}

	p, err := proxy.New(proxy.Config{
		Upstream:       upstreamURL,
		Agent:          o.agent,
		Machine:        o.machine,
		DefaultProject: o.project,
		SkipAgents:     splitList(o.skipAgents),
		Free:           o.free,
		Pricer:         pricerFn,
		Sink:           sink,
		Log:            log,
	})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", o.listen, err)
	}

	if rsink != nil {
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
					if err := rsink.Flush(fctx); err != nil && ctx.Err() == nil {
						log.Warn("proxy remote flush failed", "error", err)
					}
					cancel()
				}
			}
		}()
	}

	httpSrv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Info("proxy listening",
		"listen", "http://"+ln.Addr().String(),
		"upstream", upstreamURL.String(),
		"agent", firstString(o.agent, proxy.InferAgent(upstreamURL)),
		"remote", o.remote != "",
		"free", o.free)
	fmt.Fprintf(stdout, "dashd proxy listening on http://%s -> %s\n", ln.Addr().String(), upstreamURL.String())

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	if rsink != nil {
		if err := rsink.Flush(shutdownCtx); err != nil {
			log.Warn("final proxy flush failed", "error", err)
		}
	}
	return nil
}

func firstString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

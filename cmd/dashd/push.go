package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/scan"
	"github.com/tonym128/dashd/internal/source"
)

// remoteSink implements scan.SessionSink by shipping parsed sessions as
// gzip-compressed JSON to a remote dashd server's POST /api/ingest endpoint.
//
// Each session is buffered by ReplaceSession/AppendSession and transmitted in
// RecomputeSession; if the HTTP call fails, RecomputeSession returns an error
// so the scanner does not advance the local cursor for that file.
type remoteSink struct {
	baseURL         string
	endpoint        string
	token           string
	machine         string
	modelsPath      string
	localModelsETag string
	pricer          *source.Pricer
	log             *slog.Logger
	client          *http.Client
	pending         map[string]model.IngestSession
	lastPushAt      time.Time
}

func newRemoteSink(remoteURL, token, machine string) *remoteSink {
	base := strings.TrimRight(remoteURL, "/")
	return &remoteSink{
		baseURL:  base,
		endpoint: base + "/api/ingest",
		token:    token,
		machine:  machine,
		client:   &http.Client{Timeout: 30 * time.Second},
		pending:  map[string]model.IngestSession{},
	}
}

func (r *remoteSink) withModelsSync(modelsPath string, pricer *source.Pricer, log *slog.Logger) *remoteSink {
	r.modelsPath = modelsPath
	r.pricer = pricer
	r.log = log
	r.localModelsETag = source.ModelsFileETag(modelsPath)
	return r
}

func (r *remoteSink) ReplaceSession(sess model.SessionWrite) error {
	r.pending[sess.UID] = model.IngestSession{Mode: "replace", Session: sess}
	return nil
}

func (r *remoteSink) AppendSession(sess model.SessionWrite) error {
	r.pending[sess.UID] = model.IngestSession{Mode: "append", Session: sess}
	return nil
}

func (r *remoteSink) RecomputeSession(uid string) error {
	item, ok := r.pending[uid]
	if !ok {
		return nil
	}
	delete(r.pending, uid)
	return r.postIngest(model.IngestRequest{
		Machine:  r.machine,
		Sessions: []model.IngestSession{item},
	})
}

// SendHeartbeat sends an empty ingest payload so the remote server records
// this machine as active even when no local session logs changed.
func (r *remoteSink) SendHeartbeat() error {
	return r.postIngest(model.IngestRequest{Machine: r.machine})
}

func (r *remoteSink) postIngest(payload model.IngestRequest) error {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(payload); err != nil {
		return fmt.Errorf("encode ingest payload: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("compress ingest payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, r.endpoint, &buf)
	if err != nil {
		return fmt.Errorf("build ingest request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", r.endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("post %s: status %d: %s", r.endpoint, resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	r.lastPushAt = time.Now()
	var ack struct {
		ModelsETag string `json:"models_etag"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&ack); err == nil {
		if ack.ModelsETag != "" && ack.ModelsETag != r.localModelsETag && r.modelsPath != "" {
			r.SyncModels()
		}
	}
	return nil
}

// runPushLoop runs scanner.RunOnce on interval with exponential backoff on
// errors (capped at 5m) and a periodic 5m idle heartbeat when nothing changed.
func runPushLoop(ctx context.Context, scanner *scan.Scanner, rsink *remoteSink, interval time.Duration, log *slog.Logger) {
	const maxBackoff = 5 * time.Minute
	const heartbeatEvery = 5 * time.Minute
	backoff := interval

	for {
		err := scanner.RunOnce(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			log.Warn("push pass failed; backing off", "retry_in", backoff, "error", err)
		} else {
			backoff = interval
			if rsink.lastPushAt.IsZero() || time.Since(rsink.lastPushAt) >= heartbeatEvery {
				if hbErr := rsink.SendHeartbeat(); hbErr != nil {
					log.Debug("heartbeat failed", "error", hbErr)
				}
			}
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// SyncModels fetches the remote server's models.json if its ETag differs from
// the local copy, writes it atomically, and reloads the local Pricer.
func (r *remoteSink) SyncModels() {
	if r.modelsPath == "" || r.baseURL == "" {
		return
	}
	req, err := http.NewRequest(http.MethodGet, r.baseURL+"/api/models", nil)
	if err != nil {
		return
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	if r.localModelsETag != "" {
		req.Header.Set("If-None-Match", `"`+r.localModelsETag+`"`)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		if r.log != nil {
			r.log.Debug("sync models from server failed", "error", err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified || resp.StatusCode != http.StatusOK {
		return
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return
	}
	n, err := source.WriteModelsBytes(payload, r.modelsPath)
	if err != nil {
		if r.log != nil {
			r.log.Warn("write synced models failed", "error", err)
		}
		return
	}
	if r.pricer != nil {
		if err := r.pricer.ReloadOpenRouter(r.modelsPath); err != nil && r.log != nil {
			r.log.Warn("reload synced models failed", "error", err)
		}
	}
	r.localModelsETag = source.ModelsFileETag(r.modelsPath)
	if r.log != nil {
		r.log.Info("synced models from remote server", "models", n, "path", r.modelsPath)
	}
}

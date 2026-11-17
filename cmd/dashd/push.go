package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tonym128/dashd/internal/model"
)

// remoteSink implements scan.SessionSink by shipping parsed sessions as
// gzip-compressed JSON to a remote dashd server's POST /api/ingest endpoint.
//
// Each session is buffered by ReplaceSession/AppendSession and transmitted in
// RecomputeSession; if the HTTP call fails, RecomputeSession returns an error
// so the scanner does not advance the local cursor for that file.
type remoteSink struct {
	endpoint string
	token    string
	machine  string
	client   *http.Client
	pending  map[string]model.IngestSession
}

func newRemoteSink(remoteURL, token, machine string) *remoteSink {
	return &remoteSink{
		endpoint: strings.TrimRight(remoteURL, "/") + "/api/ingest",
		token:    token,
		machine:  machine,
		client:   &http.Client{Timeout: 30 * time.Second},
		pending:  map[string]model.IngestSession{},
	}
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

	payload := model.IngestRequest{
		Machine:  r.machine,
		Sessions: []model.IngestSession{item},
	}

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
	return nil
}

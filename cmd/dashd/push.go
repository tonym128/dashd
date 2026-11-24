package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tonym128/dashd/internal/gitlog"
	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/scan"
	"github.com/tonym128/dashd/internal/source"
	"github.com/tonym128/dashd/internal/store"
)

// Batch limits. The server accepts up to 500 sessions and 32 MiB per request;
// these stay well inside both so a batch of unusually large sessions still fits.
const (
	maxBatchSessions = 50
	maxBatchCalls    = 20000
)

// Meta keys the push agent keeps in its state database.
const (
	metaRemote        = "push.remote"
	metaInstanceID    = "push.instance_id"
	metaServerVersion = "push.server_version"
	metaLastOKAt      = "push.last_ok_at"
	metaLastError     = "push.last_error"
	metaLastErrorAt   = "push.last_error_at"
)

type redactMode int

const (
	redactNone redactMode = iota
	redactHome
	redactBasename
	redactHash
)

func parseRedactMode(s string) (redactMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none":
		return redactNone, nil
	case "home":
		return redactHome, nil
	case "basename":
		return redactBasename, nil
	case "hash":
		return redactHash, nil
	}
	return redactNone, fmt.Errorf("-redact-paths %q: want none, home, basename or hash", s)
}

// remoteSink implements scan.BatchSink by shipping parsed sessions as
// gzip-compressed JSON to a remote dashd server's POST /api/ingest endpoint.
//
// Sessions are buffered by ReplaceSession/AppendSession and sent in batches by
// Flush. The scanner only saves a file's cursor after the Flush that carried
// it succeeds, so a failed batch is simply re-read and re-sent next pass.
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
	state           *store.Store
	redact          redactMode
	redactTitles    bool
	home            string

	pending  map[string]model.IngestSession
	order    []string
	orphaned []string

	// git sends commits of the projects seen; see sendCommits.
	git        bool
	gitAt      time.Time
	lastPushAt time.Time
	// instanceChanged is set when the server reports a different database
	// than the one this agent's cursors were built against.
	instanceChanged bool
}

func newRemoteSink(remoteURL, token, machine string) *remoteSink {
	base := strings.TrimRight(remoteURL, "/")
	home, _ := os.UserHomeDir()
	return &remoteSink{
		baseURL:  base,
		endpoint: base + "/api/ingest",
		token:    token,
		machine:  machine,
		client:   &http.Client{Timeout: 60 * time.Second},
		pending:  map[string]model.IngestSession{},
		home:     home,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (r *remoteSink) withModelsSync(modelsPath string, pricer *source.Pricer, log *slog.Logger) *remoteSink {
	r.modelsPath = modelsPath
	r.pricer = pricer
	if log != nil {
		r.log = log
	}
	r.localModelsETag = source.ModelsFileETag(modelsPath)
	return r
}

func (r *remoteSink) withState(st *store.Store) *remoteSink {
	r.state = st
	return r
}

func (r *remoteSink) withRedaction(mode redactMode, titles bool) *remoteSink {
	r.redact = mode
	r.redactTitles = titles
	return r
}

// ReplaceSession buffers a full rewrite of a session.
func (r *remoteSink) ReplaceSession(sess model.SessionWrite) error {
	r.noteProject(sess.Project)
	r.queue(model.IngestSession{Mode: "replace", Session: r.redacted(sess)})
	return nil
}

// AppendSession buffers new calls for a session.
func (r *remoteSink) AppendSession(sess model.SessionWrite) error {
	r.noteProject(sess.Project)
	r.queue(model.IngestSession{Mode: "append", Session: r.redacted(sess)})
	return nil
}

// queue merges a write into the batch. Two writes for one uid in one batch
// must not overwrite each other: an append after a replace is the replace plus
// the new rows, and an append after an append is both increments.
func (r *remoteSink) queue(item model.IngestSession) {
	uid := item.Session.UID
	prev, ok := r.pending[uid]
	if !ok {
		r.pending[uid] = item
		r.order = append(r.order, uid)
		return
	}
	if item.Mode == "replace" {
		r.pending[uid] = item
		return
	}
	merged := prev
	merged.Session.Calls = append(append([]model.Call{}, prev.Session.Calls...), item.Session.Calls...)
	merged.Session.RateLimits = append(append([]model.RateLimit{}, prev.Session.RateLimits...), item.Session.RateLimits...)
	merged.Session.ToolCalls = append(append([]model.ToolCall{}, prev.Session.ToolCalls...), item.Session.ToolCalls...)
	if item.Session.Project != "" {
		merged.Session.Project = item.Session.Project
	}
	if item.Session.Title != "" {
		merged.Session.Title = item.Session.Title
	}
	r.pending[uid] = merged
}

// RecomputeSession is a no-op: the server recomputes on ingest.
func (r *remoteSink) RecomputeSession(string) error { return nil }

// MarkOrphaned queues uids whose logs vanished, so the server learns too.
func (r *remoteSink) MarkOrphaned(uids []string) error {
	r.orphaned = append(r.orphaned, uids...)
	return nil
}

// Pending reports whether the batch is full enough to send.
func (r *remoteSink) Pending() int {
	calls := 0
	for _, it := range r.pending {
		calls += len(it.Session.Calls)
	}
	if calls >= maxBatchCalls {
		return maxBatchSessions
	}
	return len(r.pending)
}

// BatchSize is the session count at which the scanner flushes.
func (r *remoteSink) BatchSize() int { return maxBatchSessions }

// Flush sends everything buffered in one request.
func (r *remoteSink) Flush() error {
	if len(r.pending) == 0 && len(r.orphaned) == 0 {
		return nil
	}
	req := model.IngestRequest{Machine: r.machine, Orphaned: r.orphaned, Version: version}
	for _, uid := range r.order {
		req.Sessions = append(req.Sessions, r.pending[uid])
	}
	r.pending = map[string]model.IngestSession{}
	r.order = nil
	r.orphaned = nil
	return r.postIngest(req)
}

// SendHeartbeat sends an empty ingest payload so the remote server records
// this machine as active even when no local session logs changed. lastErr, when
// set, tells the server this machine is up but failing.
func (r *remoteSink) SendHeartbeat(lastErr error) error {
	req := model.IngestRequest{Machine: r.machine, Version: version}
	if lastErr != nil {
		req.LastError = lastErr.Error()
	}
	return r.postIngest(req)
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
	req.Header.Set("User-Agent", "dashd-push/"+version)
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
	var ack model.IngestResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&ack); err == nil {
		r.noteServer(ack)
		if ack.ModelsETag != "" && ack.ModelsETag != r.localModelsETag && r.modelsPath != "" {
			r.SyncModels()
		}
	}
	return nil
}

// noteServer records which server database this agent is syncing to. When it
// changes, the agent's cursors describe data the server no longer has.
func (r *remoteSink) noteServer(ack model.IngestResponse) {
	if r.state == nil {
		return
	}
	if ack.ServerVersion != "" {
		_ = r.state.SetMeta(metaServerVersion, ack.ServerVersion)
	}
	if ack.InstanceID == "" {
		return
	}
	prev, _ := r.state.Meta(metaInstanceID)
	prevRemote, _ := r.state.Meta(metaRemote)
	switch {
	case prev == "":
		// First contact. If cursors already exist they were built against
		// some earlier server we cannot vouch for, so resend once.
		if n, _ := r.state.CountScanStates(); n > 0 && prevRemote != r.baseURL {
			r.instanceChanged = true
		}
	case prev != ack.InstanceID:
		r.instanceChanged = true
	}
	_ = r.state.SetMeta(metaInstanceID, ack.InstanceID)
	_ = r.state.SetMeta(metaRemote, r.baseURL)
}

// resetCursors makes the next pass resend all history.
func (r *remoteSink) resetCursors(reason string) error {
	if r.state == nil {
		return nil
	}
	r.log.Info("resending all history to the server", "reason", reason)
	r.instanceChanged = false
	return r.state.ResetScanStates()
}

// runPass runs one scan pass and handles what it learned about the server.
func (r *remoteSink) runPass(ctx context.Context, scanner *scan.Scanner) error {
	err := scanner.RunOnce(ctx)
	r.recordOutcome(err)
	if err == nil {
		if gerr := r.sendCommits(ctx); gerr != nil {
			r.log.Warn("could not send commits", "error", gerr)
		}
	}
	if r.instanceChanged {
		if rerr := r.resetCursors("server database changed (new instance id)"); rerr != nil {
			return errors.Join(err, rerr)
		}
		if err == nil {
			err = scanner.RunOnce(ctx)
			r.recordOutcome(err)
		}
	}
	return err
}

const (
	metaGitProjects = "git.projects"
	gitEvery        = 10 * time.Minute
	gitWindow       = 30 * 24 * time.Hour
	maxGitProjects  = 500
	maxCommitsSent  = 5000
)

// gitProjects is the projects sessions have been pushed from, remembered
// because a resumed session's log no longer says where it ran.
func (r *remoteSink) gitProjects() []string {
	raw, _ := r.state.Meta(metaGitProjects)
	var out []string
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func (r *remoteSink) noteProject(p string) {
	if !r.git || r.state == nil || p == "" {
		return
	}
	known := r.gitProjects()
	for _, k := range known {
		if k == p {
			return
		}
	}
	known = append(known, p)
	if len(known) > maxGitProjects {
		known = known[len(known)-maxGitProjects:]
	}
	raw, _ := json.Marshal(known)
	_ = r.state.SetMeta(metaGitProjects, string(raw))
}

// sendCommits pushes the recent commits of those projects, with the same
// redaction as everything else.
func (r *remoteSink) sendCommits(ctx context.Context) error {
	if !r.git || r.state == nil || time.Since(r.gitAt) < gitEvery {
		return nil
	}
	r.gitAt = time.Now()
	commits := gitlog.Collect(ctx, r.gitProjects(), time.Now().Add(-gitWindow))
	if len(commits) == 0 {
		return nil
	}
	if len(commits) > maxCommitsSent {
		commits = commits[:maxCommitsSent]
	}
	for i := range commits {
		commits[i].Project = r.redactPath(commits[i].Project)
		if r.redactTitles {
			commits[i].Subject = ""
		}
	}
	return r.postIngest(model.IngestRequest{Machine: r.machine, Version: version, Commits: commits})
}

func (r *remoteSink) recordOutcome(err error) {
	if r.state == nil {
		return
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	if err != nil {
		_ = r.state.SetMeta(metaLastError, err.Error())
		_ = r.state.SetMeta(metaLastErrorAt, now)
		return
	}
	_ = r.state.SetMeta(metaLastOKAt, now)
	_ = r.state.SetMeta(metaLastError, "")
}

// redacted applies -redact-paths and -redact-titles to a session before it
// leaves this machine.
func (r *remoteSink) redacted(sess model.SessionWrite) model.SessionWrite {
	if r.redactTitles {
		sess.Title = ""
		// The refusal text is the provider's, but it is free text all the same.
		limits := make([]model.RateLimit, len(sess.RateLimits))
		for i, l := range sess.RateLimits {
			limits[i] = model.RateLimit{Time: l.Time}
		}
		sess.RateLimits = limits
	}
	if r.redact == redactNone {
		return sess
	}
	sess.Project = r.redactPath(sess.Project)
	sess.Session.Project = sess.Project
	sess.Path = r.redactPath(sess.Path)
	sess.Session.Path = sess.Path
	calls := make([]model.Call, len(sess.Calls))
	for i, c := range sess.Calls {
		c.Project = r.redactPath(c.Project)
		calls[i] = c
	}
	sess.Calls = calls
	tools := make([]model.ToolCall, len(sess.ToolCalls))
	for i, t := range sess.ToolCalls {
		t.Project = r.redactPath(t.Project)
		tools[i] = t
	}
	sess.ToolCalls = tools
	return sess
}

func (r *remoteSink) redactPath(p string) string {
	if p == "" {
		return p
	}
	switch r.redact {
	case redactHome:
		if r.home != "" && (p == r.home || strings.HasPrefix(p, r.home+string(filepath.Separator)) || strings.HasPrefix(p, r.home+"/")) {
			return "~" + p[len(r.home):]
		}
		return p
	case redactBasename:
		return filepath.Base(p)
	case redactHash:
		sum := sha256.Sum256([]byte(r.machine + "\x00" + p))
		return "h:" + hex.EncodeToString(sum[:6])
	}
	return p
}

// runPushLoop runs a pass on interval with exponential backoff on errors
// (capped at 5m). A heartbeat goes out after every failed pass, carrying the
// error, and every 5m when idle, so the server can tell a failing machine from
// one that is switched off.
func runPushLoop(ctx context.Context, scanner *scan.Scanner, rsink *remoteSink, interval time.Duration, log *slog.Logger) {
	const maxBackoff = 5 * time.Minute
	const heartbeatEvery = 5 * time.Minute
	backoff := interval

	for {
		err := rsink.runPass(ctx, scanner)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			log.Warn("push pass failed; backing off", "retry_in", backoff, "error", err)
			if hbErr := rsink.SendHeartbeat(err); hbErr != nil {
				log.Debug("heartbeat failed", "error", hbErr)
			}
		} else {
			backoff = interval
			if rsink.lastPushAt.IsZero() || time.Since(rsink.lastPushAt) >= heartbeatEvery {
				if hbErr := rsink.SendHeartbeat(nil); hbErr != nil {
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
		r.log.Debug("sync models from server failed", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return
	}
	n, err := source.WriteModelsBytes(payload, r.modelsPath)
	if err != nil {
		r.log.Warn("write synced models failed", "error", err)
		return
	}
	if r.pricer != nil {
		if err := r.pricer.ReloadOpenRouter(r.modelsPath); err != nil {
			r.log.Warn("reload synced models failed", "error", err)
		}
	}
	r.localModelsETag = source.ModelsFileETag(r.modelsPath)
	r.log.Info("synced models from remote server", "models", n, "path", r.modelsPath)
}

// ---------------------------------------------------------------- auto-update

// errRestart asks the process supervisor to restart dashd into a binary that
// was just installed; runCLI turns it into exitRestart.
var errRestart = errors.New("restart requested after self-update")

// exitRestart is a non-zero code, so systemd's Restart=on-failure, launchd's
// KeepAlive and Task Scheduler's restart-on-failure all bring dashd back.
const exitRestart = 3

var restartRequested atomic.Bool

func runAutoUpdater(ctx context.Context, every time.Duration, log *slog.Logger, stop context.CancelFunc) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		updated, err := runSelfUpdate(ctx, io.Discard)
		if err != nil {
			log.Warn("auto-update check failed", "error", err)
			continue
		}
		if updated {
			log.Info("installed a new dashd release; restarting")
			restartRequested.Store(true)
			stop()
			return
		}
	}
}

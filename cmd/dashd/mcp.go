package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tonym128/dashd/internal/store"
	"github.com/tonym128/dashd/internal/web"
)

// runMCP runs a read-only Model Context Protocol (MCP) server over stdio
// (newline-delimited JSON-RPC 2.0 on stdin/stdout). When -remote is set,
// requests are forwarded to POST <remote>/mcp using -read-token (or
// -auth-token); otherwise requests run directly against the local store.
func runMCP(ctx context.Context, o options, st *store.Store, in io.Reader, out io.Writer, log *slog.Logger) error {
	var localSrv *web.Server
	var client *http.Client
	var remoteURL, token string

	if o.remote != "" {
		remoteURL = strings.TrimRight(o.remote, "/") + "/mcp"
		token = readTokenForRemote(o)
		client = &http.Client{Timeout: 60 * time.Second}
	} else {
		var err error
		localSrv, err = web.New(st, log)
		if err != nil {
			return err
		}
		localSrv.Version = version
		localSrv.DailyBudget = o.dailyBudget
		localSrv.MonthlyBudget = o.monthlyBudget
		if o.tagsFile != "" {
			localSrv.Tags, err = web.LoadTags(o.tagsFile)
			if err != nil {
				return err
			}
		}
	}

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}

		var reply []byte
		var hasReply bool

		if localSrv != nil {
			reply, hasReply = localSrv.HandleMCPMessage(ctx, line)
		} else {
			var err error
			reply, hasReply, err = forwardMCP(ctx, client, remoteURL, token, line)
			if err != nil {
				return err
			}
		}
		if hasReply {
			if _, err := out.Write(append(reply, '\n')); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

func forwardMCP(ctx context.Context, client *http.Client, endpoint, token string, body []byte) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusAccepted || len(bytes.TrimSpace(data)) == 0 {
		return nil, false, nil
	}
	if resp.StatusCode >= 400 {
		return nil, false, fmt.Errorf("remote /mcp: %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return bytes.TrimSpace(data), true, nil
}

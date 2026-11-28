package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tonym128/dashd/internal/store"
)

const (
	mcpProtocolVersion = "2025-03-26"
	maxMCPBodyBytes    = 1 << 20 // 1 MiB
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

var mcpTools = []mcpToolDef{
	{
		Name:        "get_summary",
		Description: "Get aggregated LLM usage, spend, token counts, burn windows, period comparison, and breakdowns by model, provider, project, machine, tool, and day.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"range":   map[string]any{"type": "string", "description": "Rolling range shortcut: 24h, 7d, 30d, 90d, 365d, mtd, ytd, all"},
				"from":    map[string]any{"type": "string", "description": "Start date YYYY-MM-DD (inclusive)"},
				"to":      map[string]any{"type": "string", "description": "End date YYYY-MM-DD (inclusive)"},
				"project": map[string]any{"type": "string", "description": "Filter by project path (comma-separated)"},
				"agent":   map[string]any{"type": "string", "description": "Filter by agent/harness: claude, codex, gemini, agy, opencode, pi"},
				"model":   map[string]any{"type": "string", "description": "Filter by model name (comma-separated)"},
				"machine": map[string]any{"type": "string", "description": "Filter by machine name (comma-separated)"},
				"tag":     map[string]any{"type": "string", "description": "Filter by cost-centre tag"},
			},
		},
	},
	{
		Name:        "list_sessions",
		Description: "List LLM coding sessions with cost, token counts, duration, tool calls, and timestamps.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"range":   map[string]any{"type": "string", "description": "Rolling range shortcut (e.g. 7d, 30d, all)"},
				"from":    map[string]any{"type": "string", "description": "Start date YYYY-MM-DD"},
				"to":      map[string]any{"type": "string", "description": "End date YYYY-MM-DD"},
				"project": map[string]any{"type": "string", "description": "Filter by project"},
				"agent":   map[string]any{"type": "string", "description": "Filter by agent"},
				"model":   map[string]any{"type": "string", "description": "Filter by model"},
				"machine": map[string]any{"type": "string", "description": "Filter by machine"},
				"sort":    map[string]any{"type": "string", "description": "Sort column: last_ts, cost, calls, tokens, llm_seconds"},
				"limit":   map[string]any{"type": "integer", "description": "Maximum sessions to return (default 50, max 200)"},
				"offset":  map[string]any{"type": "integer", "description": "Pagination offset"},
			},
		},
	},
	{
		Name:        "get_insights",
		Description: "Get automated cost, efficiency, cache, budget, and spend-spike insights for the filtered usage window.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"range":   map[string]any{"type": "string", "description": "Rolling range shortcut (e.g. 7d, 30d, all)"},
				"from":    map[string]any{"type": "string", "description": "Start date YYYY-MM-DD"},
				"to":      map[string]any{"type": "string", "description": "End date YYYY-MM-DD"},
				"project": map[string]any{"type": "string", "description": "Filter by project"},
				"agent":   map[string]any{"type": "string", "description": "Filter by agent"},
				"model":   map[string]any{"type": "string", "description": "Filter by model"},
				"machine": map[string]any{"type": "string", "description": "Filter by machine"},
				"tag":     map[string]any{"type": "string", "description": "Filter by tag"},
			},
		},
	},
	{
		Name:        "get_digest",
		Description: "Get a Markdown usage digest summarizing spend, top models, projects, and insights over the last N days.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"days":    map[string]any{"type": "integer", "description": "Number of days to summarize (default 7)"},
				"project": map[string]any{"type": "string", "description": "Filter by project"},
				"agent":   map[string]any{"type": "string", "description": "Filter by agent"},
				"model":   map[string]any{"type": "string", "description": "Filter by model"},
				"machine": map[string]any{"type": "string", "description": "Filter by machine"},
				"tag":     map[string]any{"type": "string", "description": "Filter by tag"},
			},
		},
	},
	{
		Name:        "get_schema",
		Description: "Return the SQLite database schema (tables, views, and columns) for writing read-only SQL queries with query_sql.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	},
	{
		Name:        "query_sql",
		Description: "Execute a single read-only SQL query (SELECT, WITH, or EXPLAIN) against the dashd SQLite database for ad-hoc analysis.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sql":   map[string]any{"type": "string", "description": "A single read-only SQL query (SELECT, WITH, or EXPLAIN)"},
				"limit": map[string]any{"type": "integer", "description": "Maximum rows to return (default 500, max 5000)"},
			},
			"required": []string{"sql"},
		},
	},
}

// handleMCP serves the Streamable HTTP MCP endpoint at POST /mcp.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMCPBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}
	resp, hasReply := s.HandleMCPMessage(r.Context(), raw)
	if !hasReply {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("MCP-Protocol-Version", mcpProtocolVersion)
	_, _ = w.Write(resp)
}

// HandleMCPMessage processes one JSON-RPC 2.0 MCP message and returns the
// encoded JSON response and whether a reply should be sent (false for
// JSON-RPC notifications). Used by both POST /mcp and `dashd mcp` (stdio).
func (s *Server) HandleMCPMessage(ctx context.Context, raw []byte) ([]byte, bool) {
	var req jsonRPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return encodeRPC(jsonRPCResponse{
			JSONRPC: "2.0",
			Error:   &jsonRPCError{Code: -32700, Message: "parse error: " + err.Error()},
		}), true
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return encodeRPC(jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonRPCError{Code: -32600, Message: "invalid jsonrpc version"},
		}), true
	}

	// Notifications carry no id and expect no reply.
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		ver := s.Version
		if ver == "" {
			ver = "dev"
		}
		return encodeRPC(jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "dashd",
					"version": ver,
				},
			},
		}), true

	case "notifications/initialized", "notifications/cancelled":
		return nil, false

	case "ping":
		if isNotification {
			return nil, false
		}
		return encodeRPC(jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		}), true

	case "tools/list":
		return encodeRPC(jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{"tools": mcpTools},
		}), true

	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return encodeRPC(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &jsonRPCError{Code: -32602, Message: "invalid params: " + err.Error()},
			}), true
		}
		result, rpcErr := s.callMCPTool(ctx, p.Name, p.Arguments)
		if rpcErr != nil {
			return encodeRPC(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   rpcErr,
			}), true
		}
		return encodeRPC(jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
		}), true

	default:
		if isNotification {
			return nil, false
		}
		return encodeRPC(jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonRPCError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)},
		}), true
	}
}

func (s *Server) callMCPTool(ctx context.Context, name string, args map[string]any) (mcpToolResult, *jsonRPCError) {
	if args == nil {
		args = map[string]any{}
	}
	switch name {
	case "get_summary":
		q := argsToQuery(args)
		filter, _, err := parseFilter(q)
		if err != nil {
			return mcpErrorResult(err.Error()), nil
		}
		data, err := s.buildPayload(ctx, filter, q)
		if err != nil {
			return mcpErrorResult(err.Error()), nil
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(data.PayloadJSON), &out); err != nil {
			return mcpErrorResult(err.Error()), nil
		}
		out["burn"] = data.Burn
		out["facets"] = data.Facets
		return mcpJSONResult(out), nil

	case "list_sessions":
		q := argsToQuery(args)
		filter, _, err := parseFilter(q)
		if err != nil {
			return mcpErrorResult(err.Error()), nil
		}
		s.applyTagFilter(ctx, &filter, q)
		limit := argInt(args, "limit", 50)
		if limit <= 0 {
			limit = 50
		}
		if limit > 200 {
			limit = 200
		}
		offset := max(0, argInt(args, "offset", 0))
		sortCol := argString(args, "sort")
		rows, total, err := s.store.SessionsPage(ctx, filter, store.SessionQuery{
			Limit:  limit,
			Offset: offset,
			Sort:   sortCol,
		})
		if err != nil {
			return mcpErrorResult(err.Error()), nil
		}
		return mcpJSONResult(map[string]any{
			"total":    total,
			"limit":    limit,
			"offset":   offset,
			"sessions": rows,
		}), nil

	case "get_insights":
		q := argsToQuery(args)
		filter, _, err := parseFilter(q)
		if err != nil {
			return mcpErrorResult(err.Error()), nil
		}
		data, err := s.buildPayload(ctx, filter, q)
		if err != nil {
			return mcpErrorResult(err.Error()), nil
		}
		insights := data.Insights
		if insights == nil {
			insights = []Insight{}
		}
		return mcpJSONResult(insights), nil

	case "get_digest":
		q := argsToQuery(args)
		filter, _, err := parseFilter(q)
		if err != nil {
			return mcpErrorResult(err.Error()), nil
		}
		s.applyTagFilter(ctx, &filter, q)
		filter.DateFrom, filter.DateTo = nil, nil
		days := argInt(args, "days", 7)
		if days < 1 || days > 3650 {
			return mcpErrorResult("days must be between 1 and 3650"), nil
		}
		md, err := BuildDigest(ctx, s.store, DigestOptions{
			Filter:        filter,
			Days:          days,
			DailyBudget:   s.DailyBudget,
			MonthlyBudget: s.MonthlyBudget,
			Now:           s.Generated(),
		})
		if err != nil {
			return mcpErrorResult(err.Error()), nil
		}
		return mcpToolResult{Content: []mcpContent{{Type: "text", Text: md}}}, nil

	case "get_schema":
		tables, err := s.store.Schema(ctx)
		if err != nil {
			return mcpErrorResult(err.Error()), nil
		}
		return mcpJSONResult(map[string]any{"tables": tables}), nil

	case "query_sql":
		sqlText := argString(args, "sql")
		limit := argInt(args, "limit", store.DefaultQueryMaxRows)
		res, err := s.store.QueryReadOnly(ctx, sqlText, limit)
		if err != nil {
			return mcpErrorResult(err.Error()), nil
		}
		return mcpJSONResult(res), nil

	default:
		return mcpToolResult{}, &jsonRPCError{Code: -32602, Message: fmt.Sprintf("unknown tool: %s", name)}
	}
}

func argsToQuery(args map[string]any) url.Values {
	q := url.Values{}
	for _, k := range []string{"range", "from", "to", "project", "agent", "model", "machine", "tag"} {
		if v := strings.TrimSpace(argString(args, k)); v != "" {
			q.Set(k, v)
		}
	}
	return q
}

func argString(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func argInt(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return parsed
		}
	}
	return def
}

func mcpJSONResult(v any) mcpToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcpErrorResult(err.Error())
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: string(b)}}}
}

func mcpErrorResult(msg string) mcpToolResult {
	return mcpToolResult{
		Content: []mcpContent{{Type: "text", Text: msg}},
		IsError: true,
	}
}

func encodeRPC(resp jsonRPCResponse) []byte {
	b, _ := json.Marshal(resp)
	return b
}
